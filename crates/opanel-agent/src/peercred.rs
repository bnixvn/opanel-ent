//! Who is on the other end of the socket.

use std::io;
use std::os::unix::io::AsRawFd;
use std::os::unix::net::UnixStream;

/// The process on the other end of a unix socket.
#[derive(Debug, Clone, Copy)]
pub struct Credential {
    pub uid: u32,
    pub gid: u32,
    pub pid: i32,
}

/// Reads SO_PEERCRED.
///
/// The kernel fills these in at connect time from the calling process's real
/// identity. A client cannot set or spoof them, which is what makes the
/// socket an authentication boundary rather than a convention.
pub fn of(conn: &UnixStream) -> io::Result<Credential> {
    let mut cred = libc::ucred { pid: 0, uid: 0, gid: 0 };
    let mut len = std::mem::size_of::<libc::ucred>() as libc::socklen_t;

    // SAFETY: the fd is owned by `conn` and outlives the call; `cred` and
    // `len` are correctly sized for SO_PEERCRED, which is the only option
    // being asked for.
    let rc = unsafe {
        libc::getsockopt(
            conn.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            (&raw mut cred).cast::<libc::c_void>(),
            &raw mut len,
        )
    };
    if rc != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(Credential { uid: cred.uid, gid: cred.gid, pid: cred.pid })
}
