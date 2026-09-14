//! The host, wrapped.
//!
//! One module per external tool. Actions state what they want done; these
//! modules know which binary does it and how it reports, so an action never
//! parses command output itself.
//!
//! Each wrapper is written once and completely, rather than growing a
//! function at a time as actions need them: the unused half of `svc` is the
//! half the next slice of the port calls, and a partial wrapper is how two
//! callers end up spelling "enable a unit" two different ways.
#![allow(dead_code)]

pub mod pkgmgr;
pub mod run;
pub mod svc;
