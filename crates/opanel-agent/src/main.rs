//! opanel-agent: the privileged side of OPanel.
//!
//! It runs as root, listens on a unix socket, and executes a fixed set of
//! named actions. The panel runs unprivileged and can do nothing to the host
//! on its own; it asks for each privileged step by name. Every action is a
//! typed function with a validated input, and no action composes a shell
//! string -- which removes quoting, word splitting and injection from the
//! threat model rather than filtering for them.

mod actions;
mod peercred;
mod protocol;
mod registry;
mod server;
mod version;

use registry::Registry;
use std::process::ExitCode;

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().collect();

    if args.iter().any(|a| a == "--version") {
        println!("opanel-agent {}", version::string());
        return ExitCode::SUCCESS;
    }

    let socket = flag(&args, "--socket").unwrap_or_else(|| protocol::SOCKET_PATH.to_owned());
    let api_user = flag(&args, "--api-user").unwrap_or_else(|| "opanel".to_owned());

    let mut registry = Registry::new();
    actions::register_all(&mut registry);
    actions::core::publish_table(registry.names());
    let count = registry.len();

    // root can always call, for opanelctl recovery; the panel's own account
    // is looked up rather than assumed.
    let mut uids = vec![0u32];
    let gid = match lookup_user(&api_user) {
        Some((uid, gid)) => {
            uids.push(uid);
            Some(gid)
        }
        None => {
            eprintln!("opanel-agent: no account named {api_user:?}; only root may call");
            None
        }
    };

    let listener = match server::Server::listen(&socket, gid) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("opanel-agent: listen {socket}: {e}");
            return ExitCode::FAILURE;
        }
    };
    eprintln!(
        "opanel-agent: listening socket={socket} version={} allowed_uids={uids:?} actions={count}",
        version::string()
    );

    server::Server::new(registry, uids).serve(&listener);
    ExitCode::SUCCESS
}

/// Reads `--name value` or `--name=value`.
fn flag(args: &[String], name: &str) -> Option<String> {
    let prefix = format!("{name}=");
    let mut it = args.iter();
    while let Some(a) = it.next() {
        if a == name {
            return it.next().cloned();
        }
        if let Some(rest) = a.strip_prefix(&prefix) {
            return Some(rest.to_owned());
        }
    }
    None
}

/// Resolves an account to its uid and gid from the passwd database.
fn lookup_user(name: &str) -> Option<(u32, u32)> {
    let c_name = std::ffi::CString::new(name).ok()?;
    // SAFETY: the pointer outlives the call. getpwnam returns a pointer into
    // static storage, which is read immediately and never retained.
    let pw = unsafe { libc::getpwnam(c_name.as_ptr()) };
    if pw.is_null() {
        return None;
    }
    let pw = unsafe { &*pw };
    Some((pw.pw_uid, pw.pw_gid))
}
