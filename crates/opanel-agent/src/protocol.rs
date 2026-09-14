//! The wire format between the panel and the agent.
//!
//! One JSON object, a newline, one reply, and the connection closes. Kept
//! byte-compatible with the Go implementation on purpose: the two run side by
//! side while the port is in progress, and the panel cannot tell them apart.

use serde::{Deserialize, Serialize};
use std::io::{self, BufRead, Read, Write};

/// The default unix socket. Mode 0660, owned root:opanel.
pub const SOCKET_PATH: &str = "/run/opanel/agent.sock";

/// Caps a single request or response. Actions move config text, not files;
/// anything larger is a bug or an attack.
pub const MAX_MESSAGE_BYTES: usize = 4 << 20;

/// One call.
#[derive(Debug, Deserialize)]
pub struct Request {
    /// Echoed back so a caller can correlate logs. No protocol meaning.
    #[serde(default)]
    pub id: String,
    /// Names the handler, e.g. "ping" or "systemd.restart".
    pub action: String,
    /// Pins the payload shape, so a stale panel fails loudly rather than
    /// being misread by a newer agent.
    #[serde(default)]
    pub version: i64,
    /// Action-specific input, decoded by the handler.
    #[serde(default)]
    pub payload: Option<serde_json::Value>,
}

/// The single reply.
#[derive(Debug, Serialize)]
pub struct Response {
    #[serde(skip_serializing_if = "String::is_empty")]
    pub id: String,
    pub ok: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub code: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub payload: Option<serde_json::Value>,
}

impl Response {
    pub fn ok(id: &str, payload: serde_json::Value) -> Self {
        Self { id: id.to_owned(), ok: true, error: String::new(), code: String::new(), payload: Some(payload) }
    }

    pub fn failed(id: &str, code: Code, message: impl Into<String>) -> Self {
        Self {
            id: id.to_owned(),
            ok: false,
            error: message.into(),
            code: code.as_str().to_owned(),
            payload: None,
        }
    }
}

/// Returned in `Response.code` so callers branch without matching messages.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Code {
    UnknownAction,
    VersionMismatch,
    BadPayload,
    Denied,
    Internal,
    Timeout,
}

impl Code {
    pub fn as_str(self) -> &'static str {
        match self {
            Code::UnknownAction => "unknown_action",
            Code::VersionMismatch => "version_mismatch",
            Code::BadPayload => "bad_payload",
            Code::Denied => "denied",
            Code::Internal => "internal",
            Code::Timeout => "timeout",
        }
    }
}

/// Reads one newline-terminated request, refusing anything oversized before
/// it is allocated rather than after.
pub fn read_request<R: BufRead>(r: &mut R) -> io::Result<Request> {
    let mut line = Vec::new();
    // by_ref, because Read::take consumes the reader it is called on and the
    // caller still owns this one.
    r.by_ref()
        .take(MAX_MESSAGE_BYTES as u64 + 1)
        .read_until(b'\n', &mut line)?;
    if line.len() > MAX_MESSAGE_BYTES {
        return Err(io::Error::new(io::ErrorKind::InvalidData, "request too large"));
    }
    if line.is_empty() {
        return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "empty request"));
    }
    serde_json::from_slice(&line).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))
}

/// Writes one reply, newline-terminated.
pub fn write_response<W: Write>(w: &mut W, resp: &Response) -> io::Result<()> {
    let mut body = serde_json::to_vec(resp)?;
    body.push(b'\n');
    w.write_all(&body)?;
    w.flush()
}
