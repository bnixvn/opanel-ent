//! The set of things the agent will do, and nothing else.
//!
//! An action is looked up by name and version. There is no path from a
//! request to a shell, and no way to add an action at runtime: the table is
//! built at startup and then only read.

use crate::protocol::{Code, Request, Response};
use serde_json::Value;
use std::collections::BTreeMap;
use std::time::Duration;

/// What an action returns: a JSON payload, or a coded failure.
pub type Outcome = Result<Value, Failure>;

/// A failure with the code the caller will branch on.
#[derive(Debug)]
pub struct Failure {
    pub code: Code,
    pub message: String,
}

impl Failure {
    pub fn new(code: Code, message: impl Into<String>) -> Self {
        Self { code, message: message.into() }
    }

    pub fn internal(message: impl Into<String>) -> Self {
        Self::new(Code::Internal, message)
    }

    pub fn bad_payload(message: impl Into<String>) -> Self {
        Self::new(Code::BadPayload, message)
    }
}

impl From<std::io::Error> for Failure {
    fn from(e: std::io::Error) -> Self {
        Failure::internal(e.to_string())
    }
}

/// One registered operation.
pub struct Action {
    pub version: i64,
    /// Overrides the server's default budget. Zero means use it.
    pub timeout: Duration,
    pub run: Box<dyn Fn(Option<Value>) -> Outcome + Send + Sync>,
}

/// The table.
#[derive(Default)]
pub struct Registry {
    actions: BTreeMap<String, Action>,
}

impl Registry {
    pub fn new() -> Self {
        Self::default()
    }

    /// Registers an action. Panics on a duplicate name: two handlers for one
    /// name is a build mistake, and a root daemon should not start with one.
    pub fn register<F>(&mut self, name: &str, version: i64, run: F)
    where
        F: Fn(Option<Value>) -> Outcome + Send + Sync + 'static,
    {
        self.register_slow(name, version, Duration::ZERO, run);
    }

    /// `register` for an action that legitimately takes longer than the
    /// default budget -- archiving a home directory, dumping a database.
    pub fn register_slow<F>(&mut self, name: &str, version: i64, timeout: Duration, run: F)
    where
        F: Fn(Option<Value>) -> Outcome + Send + Sync + 'static,
    {
        let existing = self
            .actions
            .insert(name.to_owned(), Action { version, timeout, run: Box::new(run) });
        assert!(existing.is_none(), "action registered twice: {name}");
    }

    /// What this build serves, so the panel can detect a version skew rather
    /// than guessing from its own compiled-in list.
    pub fn names(&self) -> BTreeMap<String, i64> {
        self.actions.iter().map(|(k, a)| (k.clone(), a.version)).collect()
    }

    pub fn len(&self) -> usize {
        self.actions.len()
    }

    /// The budget this action gets, falling back to the server's default.
    pub fn budget(&self, name: &str, default: Duration) -> Duration {
        match self.actions.get(name) {
            Some(a) if !a.timeout.is_zero() => a.timeout,
            _ => default,
        }
    }

    /// Looks the action up and runs it.
    pub fn dispatch(&self, req: &Request) -> Response {
        let Some(action) = self.actions.get(&req.action) else {
            return Response::failed(
                &req.id,
                Code::UnknownAction,
                format!("unknown action {:?}", req.action),
            );
        };
        // Zero means "whatever the agent has", matching the Go agent: it is
        // for opanelctl and for anybody holding the socket open by hand.
        // Every call from the panel pins a version, which is where the
        // guarantee is needed and where it applies.
        if req.version != 0 && action.version != req.version {
            return Response::failed(
                &req.id,
                Code::VersionMismatch,
                format!(
                    "action {:?} is version {} here, caller asked for {}",
                    req.action, action.version, req.version
                ),
            );
        }
        match (action.run)(req.payload.clone()) {
            Ok(payload) => Response::ok(&req.id, payload),
            Err(f) => Response::failed(&req.id, f.code, f.message),
        }
    }
}
