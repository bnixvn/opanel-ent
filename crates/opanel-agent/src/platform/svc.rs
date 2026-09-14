//! Controlling systemd units.

use super::run::{self, Options};
use serde::Serialize;
use std::time::Duration;

const SYSTEMCTL: &str = "systemctl";

/// Starting a unit that fails slowly, or stopping one that flushes to disk,
/// can take longer than the default command budget.
const OPERATION_TIMEOUT: Duration = Duration::from_secs(120);

/// Reports whether name is a syntactically acceptable unit name.
///
/// Guards against a caller-supplied unit name turning into extra systemctl
/// arguments. Even with argv execution, a name like "--now" would be read as
/// a flag, so the shape is validated rather than the quoting.
pub fn valid_unit(name: &str) -> bool {
    let mut chars = name.chars();
    match chars.next() {
        Some(c) if c.is_ascii_alphanumeric() => {}
        _ => return false,
    }
    if name.len() > 128 {
        return false;
    }
    chars.all(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '.' | '@' | '-'))
}

/// A unit's current state.
#[derive(Debug, Clone, Serialize)]
pub struct Status {
    pub unit: String,
    /// active|inactive|failed|activating|unknown
    pub active: String,
    /// enabled|disabled|static|masked|not-found
    pub enabled: String,
}

impl Status {
    /// Reports whether the unit is active.
    pub fn running(&self) -> bool {
        self.active == "active"
    }
}

fn check(unit: &str) -> run::Result<()> {
    if !valid_unit(unit) {
        return Err(run::Error::refused(format!("svc: invalid unit name {unit:?}")));
    }
    Ok(())
}

/// Reports a unit's active and enabled state.
///
/// systemctl exits non-zero for inactive or disabled units, which is
/// information rather than an error, so the exit code is ignored and the
/// printed word is used.
pub fn get(unit: &str) -> run::Result<Status> {
    check(unit)?;
    let mut st = Status {
        unit: unit.to_owned(),
        active: "unknown".to_owned(),
        enabled: "unknown".to_owned(),
    };

    if let Some(word) = word(&[SYSTEMCTL, "is-active", unit], &[1, 3, 4]) {
        st.active = word;
    }
    if let Some(word) = word(&[SYSTEMCTL, "is-enabled", unit], &[1]) {
        st.enabled = word;
    }
    Ok(st)
}

/// Runs a query whose answer is the word it prints, whatever it exits with.
fn word(argv: &[&str], allowed: &[i32]) -> Option<String> {
    let res = match run::cmd_opts(argv, Options::new().allow_exit(allowed)) {
        Ok(res) => res,
        // An exit outside the allowed set still printed the state in every
        // case that matters here; the Go agent reads it the same way.
        Err(e) => e.result,
    };
    let text = res.text();
    if text.is_empty() {
        None
    } else {
        Some(text.to_owned())
    }
}

fn do_verb(verb: &str, unit: &str) -> run::Result<()> {
    check(unit)?;
    run::cmd_opts(&[SYSTEMCTL, verb, unit], Options::new().timeout(OPERATION_TIMEOUT))?;
    Ok(())
}

pub fn start(unit: &str) -> run::Result<()> {
    do_verb("start", unit)
}

pub fn stop(unit: &str) -> run::Result<()> {
    do_verb("stop", unit)
}

pub fn restart(unit: &str) -> run::Result<()> {
    do_verb("restart", unit)
}

pub fn reload(unit: &str) -> run::Result<()> {
    do_verb("reload", unit)
}

/// Reloads when the unit supports it and restarts otherwise.
pub fn reload_or_restart(unit: &str) -> run::Result<()> {
    do_verb("reload-or-restart", unit)
}

/// Turns on boot activation, optionally starting the unit now.
pub fn enable(unit: &str, now: bool) -> run::Result<()> {
    toggle("enable", unit, now)
}

/// Turns off boot activation, optionally stopping the unit now.
pub fn disable(unit: &str, now: bool) -> run::Result<()> {
    toggle("disable", unit, now)
}

fn toggle(verb: &str, unit: &str, now: bool) -> run::Result<()> {
    check(unit)?;
    let mut argv = vec![SYSTEMCTL, verb];
    if now {
        argv.push("--now");
    }
    argv.push(unit);
    run::cmd_opts(&argv, Options::new().timeout(OPERATION_TIMEOUT))?;
    Ok(())
}

/// Re-reads unit files after the panel writes one.
pub fn daemon_reload() -> run::Result<()> {
    run::cmd_opts(&[SYSTEMCTL, "daemon-reload"], Options::new().timeout(OPERATION_TIMEOUT))?;
    Ok(())
}

/// Returns the last n lines for a unit.
pub fn journal(unit: &str, lines: i64) -> run::Result<String> {
    check(unit)?;
    let lines = if !(1..=5000).contains(&lines) { 200 } else { lines };
    let n = lines.to_string();
    let res = run::cmd(&["journalctl", "-u", unit, "-n", &n, "--no-pager", "--output", "short-iso"])?;
    Ok(res.stdout)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unit_names_are_checked_by_shape() {
        for good in ["mariadb", "clamd@scan", "opanel-api", "a", "x_1.service"] {
            assert!(valid_unit(good), "{good} should be accepted");
        }
        // The first three are the ones that matter: a leading dash becomes a
        // systemctl flag, a space becomes two arguments, and a semicolon is
        // what a reader assumes is dangerous even though there is no shell.
        for bad in ["--now", "-x", "two words", "a;b", "", "../etc/passwd", &"a".repeat(129)] {
            assert!(!valid_unit(bad), "{bad:?} should be refused");
        }
    }
}
