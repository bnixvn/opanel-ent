//! Every privileged operation, one module per subject area.

pub mod core;
pub mod sysstat;

use crate::registry::Registry;

/// Wires every action into the registry. The table is complete before the
/// listener opens, so a request can never race registration.
pub fn register_all(r: &mut Registry) {
    core::register(r);
    sysstat::register(r);
}
