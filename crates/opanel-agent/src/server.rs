//! The listener: one request per connection, behind a peer-credential check.

use crate::peercred;
use crate::protocol::{self, Code, Response};
use crate::registry::Registry;
use std::io::{BufReader, BufWriter};
use std::sync::mpsc;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::Path;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

/// Bounds one handler. An action may override it; see `Registry::budget`.
pub const DEFAULT_ACTION_TIMEOUT: Duration = Duration::from_secs(120);

/// How many handlers may run at once. A root daemon that will start an
/// unbounded number of subprocesses on request is a fork bomb with a socket.
const MAX_CONCURRENT: usize = 16;

pub struct Server {
    registry: Arc<Registry>,
    allowed_uids: Vec<u32>,
    inflight: Arc<AtomicUsize>,
}

impl Server {
    pub fn new(registry: Registry, allowed_uids: Vec<u32>) -> Self {
        Self {
            registry: Arc::new(registry),
            allowed_uids,
            inflight: Arc::new(AtomicUsize::new(0)),
        }
    }

    /// Binds the socket, replacing one left by an unclean shutdown.
    ///
    /// Order matters: the group is widened before the mode is relaxed, so
    /// there is no window in which the socket is group-writable by the wrong
    /// group.
    pub fn listen(path: &str, gid: Option<u32>) -> std::io::Result<UnixListener> {
        if let Some(dir) = Path::new(path).parent() {
            std::fs::create_dir_all(dir)?;
            // The directory must be traversable by the panel's group, not
            // just the socket readable: connect() fails with EACCES either
            // way, which reads as a socket problem and is not.
            if let Some(gid) = gid {
                chown(dir, 0, gid)?;
                std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o750))?;
            }
        }
        match std::fs::remove_file(path) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        }

        let listener = UnixListener::bind(path)?;
        if let Some(gid) = gid {
            chown(Path::new(path), 0, gid)?;
        }
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o660))?;
        Ok(listener)
    }

    /// Accepts until the listener is closed.
    pub fn serve(&self, listener: &UnixListener) {
        for conn in listener.incoming() {
            let conn = match conn {
                Ok(c) => c,
                // A single bad accept must not kill the agent: the panel
                // would lose every privileged operation at once.
                Err(e) => {
                    eprintln!("agent: accept failed: {e}");
                    continue;
                }
            };
            let registry = Arc::clone(&self.registry);
            let allowed = self.allowed_uids.clone();
            let inflight = Arc::clone(&self.inflight);

            if inflight.load(Ordering::SeqCst) >= MAX_CONCURRENT {
                let mut w = BufWriter::new(&conn);
                let _ = protocol::write_response(
                    &mut w,
                    &Response::failed("", Code::Internal, "the agent is busy"),
                );
                continue;
            }
            inflight.fetch_add(1, Ordering::SeqCst);
            std::thread::spawn(move || {
                handle(&conn, registry, &allowed);
                inflight.fetch_sub(1, Ordering::SeqCst);
            });
        }
    }
}

fn handle(conn: &UnixStream, registry: Arc<Registry>, allowed: &[u32]) {
    // A caller that connects and then stalls must not hold a slot for ever.
    let _ = conn.set_read_timeout(Some(DEFAULT_ACTION_TIMEOUT + Duration::from_secs(30)));
    let _ = conn.set_write_timeout(Some(Duration::from_secs(30)));

    let cred = match peercred::of(conn) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("agent: cannot read peer credentials: {e}");
            reply(conn, &Response::failed("", Code::Denied, "peer identity unavailable"));
            return;
        }
    };
    if !allowed.contains(&cred.uid) {
        eprintln!("agent: rejected peer uid={} pid={}", cred.uid, cred.pid);
        reply(conn, &Response::failed("", Code::Denied, "caller not permitted"));
        return;
    }

    let mut r = BufReader::new(conn);
    let req = match protocol::read_request(&mut r) {
        Ok(req) => req,
        Err(e) => {
            reply(conn, &Response::failed("", Code::BadPayload, e.to_string()));
            return;
        }
    };

    // Now that the action is known, give the connection the budget that
    // action actually has. The deadline set above was a guess made before
    // reading the request, and it is too short for a backup.
    let budget = registry.budget(&req.action, DEFAULT_ACTION_TIMEOUT);
    let _ = conn.set_read_timeout(Some(budget + Duration::from_secs(30)));
    let _ = conn.set_write_timeout(Some(budget + Duration::from_secs(30)));

    let started = Instant::now();
    let resp = run_within(registry, &req, budget);
    let ok = resp.ok;
    reply(conn, &resp);

    // The agent's own record of every privileged action, written on the root
    // side so a compromised panel cannot omit entries from it.
    eprintln!(
        "agent: action={} ok={} uid={} pid={} ms={}",
        req.action,
        ok,
        cred.uid,
        cred.pid,
        started.elapsed().as_millis()
    );
}

/// Runs the action, answering with a timeout rather than holding the caller
/// once the budget is spent.
///
/// The handler keeps running after that: there is no cancellation to deliver
/// it, and killing a thread mid-write is how a config file ends up truncated.
/// Every command it spawns carries its own budget, so it ends on its own; the
/// caller simply stops waiting, which is what it needed.
fn run_within(registry: Arc<Registry>, req: &protocol::Request, budget: Duration) -> Response {
    let (tx, rx) = mpsc::channel();
    let owned = req.clone();
    std::thread::spawn(move || {
        let _ = tx.send(registry.dispatch(&owned));
    });
    match rx.recv_timeout(budget) {
        Ok(resp) => resp,
        Err(_) => Response::failed(&req.id, Code::Timeout, "action timed out"),
    }
}

fn reply(conn: &UnixStream, resp: &Response) {
    let mut w = BufWriter::new(conn);
    if let Err(e) = protocol::write_response(&mut w, resp) {
        eprintln!("agent: could not write reply: {e}");
    }
}

fn chown(path: &Path, uid: u32, gid: u32) -> std::io::Result<()> {
    let c = std::ffi::CString::new(path.as_os_str().as_encoded_bytes())
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidInput, e))?;
    // SAFETY: the pointer is a valid NUL-terminated path that outlives the
    // call, and chown writes nothing back through it.
    if unsafe { libc::chown(c.as_ptr(), uid, gid) } != 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}
