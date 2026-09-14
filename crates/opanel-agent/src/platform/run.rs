//! Running external commands.
//!
//! Every entry point takes an argv slice. There is deliberately no helper
//! that accepts a command line as one string: the absence of a shell is what
//! makes caller-supplied values (domain names, usernames, package names) safe
//! to pass through without quoting rules.

use std::fmt;
use std::io::{Read, Write};
use std::process::{Child, Command, ExitStatus, Stdio};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

/// Bounds a command that does not ask for its own budget.
pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(60);

/// The outcome of one command.
#[derive(Debug, Default, Clone)]
pub struct Output {
    pub args: Vec<String>,
    pub exit_code: i32,
    pub stdout: String,
    pub stderr: String,
    pub duration: Duration,
}

impl Output {
    /// Reports a non-zero exit.
    pub fn failed(&self) -> bool {
        self.exit_code != 0
    }

    /// Returns stdout, or stderr when stdout is empty. Useful for tools that
    /// report on the wrong stream.
    pub fn text(&self) -> &str {
        let s = self.stdout.trim();
        if !s.is_empty() {
            s
        } else {
            self.stderr.trim()
        }
    }
}

/// A command that could not be run, or that exited non-zero without that code
/// being allowed. The `Output` is still filled in, so a caller can inspect
/// what a failure printed.
#[derive(Debug)]
pub struct Error {
    pub result: Output,
    /// Set when the failure was not the exit code itself: a missing binary, a
    /// timeout, a broken pipe.
    pub cause: Option<String>,
}

impl Error {
    /// A refusal raised before anything was executed, so callers of this
    /// module have one error type rather than two.
    pub fn refused(message: impl Into<String>) -> Self {
        Self { result: Output::default(), cause: Some(message.into()) }
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        if self.result.args.is_empty() {
            return write!(f, "{}", self.cause.as_deref().unwrap_or("run failed"));
        }
        write!(f, "run {}: exit {}", self.result.args.join(" "), self.result.exit_code)?;
        let err = self.result.stderr.trim();
        if !err.is_empty() {
            write!(f, ": {}", err.lines().next().unwrap_or(""))?;
        }
        if let Some(cause) = &self.cause {
            write!(f, ": {cause}")?;
        }
        Ok(())
    }
}

impl std::error::Error for Error {}

pub type Result<T> = std::result::Result<T, Error>;

/// One invocation's settings.
pub struct Options {
    timeout: Duration,
    stdin: Option<String>,
    env: Vec<(String, String)>,
    dir: Option<String>,
    /// Exit codes to treat as success besides 0. dnf uses 100 for "updates
    /// available", which is information rather than failure.
    ok_codes: Vec<i32>,
}

impl Default for Options {
    fn default() -> Self {
        Self {
            timeout: DEFAULT_TIMEOUT,
            stdin: None,
            env: Vec::new(),
            dir: None,
            ok_codes: Vec::new(),
        }
    }
}

impl Options {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn timeout(mut self, d: Duration) -> Self {
        self.timeout = d;
        self
    }

    pub fn stdin(mut self, s: impl Into<String>) -> Self {
        self.stdin = Some(s.into());
        self
    }

    pub fn env(mut self, key: impl Into<String>, value: impl Into<String>) -> Self {
        self.env.push((key.into(), value.into()));
        self
    }

    pub fn dir(mut self, d: impl Into<String>) -> Self {
        self.dir = Some(d.into());
        self
    }

    /// Marks extra exit codes as success.
    pub fn allow_exit(mut self, codes: &[i32]) -> Self {
        self.ok_codes.extend_from_slice(codes);
        self
    }
}

/// Runs argv with the default budget.
pub fn cmd(argv: &[&str]) -> Result<Output> {
    cmd_opts(argv, Options::new())
}

/// Runs argv and returns its result.
pub fn cmd_opts(argv: &[&str], o: Options) -> Result<Output> {
    let Some((bin, args)) = argv.split_first() else {
        return Err(Error::refused("run: empty argv"));
    };
    let mut out = Output {
        args: argv.iter().map(|s| (*s).to_owned()).collect(),
        ..Default::default()
    };

    let mut command = Command::new(bin);
    command
        .args(args)
        .stdin(if o.stdin.is_some() { Stdio::piped() } else { Stdio::null() })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    for (k, v) in &o.env {
        command.env(k, v);
    }
    if let Some(dir) = &o.dir {
        command.current_dir(dir);
    }

    let started = Instant::now();
    let mut child = match command.spawn() {
        Ok(c) => c,
        Err(e) => {
            out.exit_code = -1;
            out.duration = started.elapsed();
            return Err(Error { result: out, cause: Some(e.to_string()) });
        }
    };

    // The pipes are drained on their own threads. A command that writes more
    // than a pipe buffer holds while nobody reads, and dnf writes far more
    // than a pipe buffer.
    let stdout_thread = child.stdout.take().map(drain);
    let stderr_thread = child.stderr.take().map(drain);
    if let (Some(data), Some(mut pipe)) = (o.stdin.clone(), child.stdin.take()) {
        std::thread::spawn(move || {
            let _ = pipe.write_all(data.as_bytes());
        });
    }

    let (status, timed_out) = wait_within(&mut child, o.timeout);

    out.stdout = collect(stdout_thread);
    out.stderr = collect(stderr_thread);
    out.duration = started.elapsed();
    out.exit_code = status.map(exit_code_of).unwrap_or(-1);

    if timed_out {
        return Err(Error { result: out, cause: Some(format!("timed out after {:?}", o.timeout)) });
    }
    if out.exit_code != 0 && !o.ok_codes.contains(&out.exit_code) {
        return Err(Error { result: out, cause: None });
    }
    Ok(out)
}

fn drain<R: Read + Send + 'static>(mut pipe: R) -> JoinHandle<Vec<u8>> {
    std::thread::spawn(move || {
        let mut buf = Vec::new();
        let _ = pipe.read_to_end(&mut buf);
        buf
    })
}

/// Waits for the child, killing it when the budget runs out.
///
/// Polled rather than waited on a second thread: `try_wait` keeps ownership of
/// the child here, so the kill can never land on a pid that has already been
/// reaped and handed out again. The interval backs off, so a twenty-minute
/// dnf transaction costs a handful of wakeups a second rather than hundreds.
fn wait_within(child: &mut Child, budget: Duration) -> (Option<ExitStatus>, bool) {
    let deadline = Instant::now() + budget;
    let mut interval = Duration::from_millis(2);
    loop {
        match child.try_wait() {
            Ok(Some(status)) => return (Some(status), false),
            Ok(None) => {}
            Err(_) => return (None, false),
        }
        let now = Instant::now();
        if now >= deadline {
            let _ = child.kill();
            return (child.wait().ok(), true);
        }
        std::thread::sleep(interval.min(deadline - now));
        interval = (interval * 2).min(Duration::from_millis(100));
    }
}

fn collect(handle: Option<JoinHandle<Vec<u8>>>) -> String {
    match handle.and_then(|h| h.join().ok()) {
        Some(buf) => String::from_utf8_lossy(&buf).into_owned(),
        None => String::new(),
    }
}

/// The exit code, or 128 + signal for a killed process, which is the shell
/// convention and the one every log reader already knows.
fn exit_code_of(status: ExitStatus) -> i32 {
    use std::os::unix::process::ExitStatusExt;
    match status.code() {
        Some(c) => c,
        None => status.signal().map(|s| 128 + s).unwrap_or(-1),
    }
}

/// Finds a binary on PATH.
pub fn look(bin: &str) -> Option<String> {
    let path = std::env::var("PATH").unwrap_or_else(|_| "/usr/sbin:/usr/bin:/sbin:/bin".to_owned());
    for dir in path.split(':').filter(|d| !d.is_empty()) {
        let candidate = std::path::Path::new(dir).join(bin);
        if candidate.is_file() {
            return Some(candidate.to_string_lossy().into_owned());
        }
    }
    None
}
