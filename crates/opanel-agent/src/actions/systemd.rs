//! Driving the services the panel owns, and no others.

use crate::platform::svc::{self, Status};
use crate::registry::{decode, Failure, Outcome, Registry, Validate};
use serde::{Deserialize, Serialize};
use serde_json::json;

/// The units the panel may touch.
///
/// An allowlist rather than a shape check: the agent runs as root, so
/// "restart any unit you can name" is a different grant from "restart the
/// web server", and only the second one is wanted here.
const MANAGED_UNITS: &[&str] = &[
    "lshttpd",
    "lsws",
    "openlitespeed",
    "mariadb",
    "valkey",
    "nftables",
    "opanel-api",
    "opanel-agent",
    "sshd",
    "crond",
    "clamd@scan",
];

/// Spelled the way the Go agent formats the same list, so a caller that
/// matched on the message keeps working across the port.
const UNIT_VERBS: &str = "[start stop restart reload reload-or-restart]";

fn unit_allowed(name: &str) -> bool {
    MANAGED_UNITS.contains(&name)
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct UnitRequest {
    pub unit: String,
}

impl Validate for UnitRequest {
    fn validate(&self) -> Result<(), String> {
        if !svc::valid_unit(&self.unit) {
            return Err(format!("unit {:?} is not a valid unit name", self.unit));
        }
        if !unit_allowed(&self.unit) {
            return Err(format!("unit {:?} is not managed by opanel", self.unit));
        }
        Ok(())
    }
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct UnitActionRequest {
    pub unit: String,
    pub action: String,
}

impl Validate for UnitActionRequest {
    fn validate(&self) -> Result<(), String> {
        UnitRequest { unit: self.unit.clone() }.validate()?;
        match self.action.as_str() {
            "start" | "stop" | "restart" | "reload" | "reload-or-restart" => Ok(()),
            other => Err(format!("action {other:?} is not one of {UNIT_VERBS}")),
        }
    }
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct JournalRequest {
    pub unit: String,
    #[serde(default)]
    pub lines: i64,
}

impl Validate for JournalRequest {
    fn validate(&self) -> Result<(), String> {
        UnitRequest { unit: self.unit.clone() }.validate()
    }
}

#[derive(Serialize)]
struct StatusList {
    units: Vec<Status>,
}

pub fn register(r: &mut Registry) {
    r.register("systemd.status", 1, |payload| -> Outcome {
        let req: UnitRequest = decode(payload)?;
        let status = svc::get(&req.unit)?;
        serde_json::to_value(status).map_err(|e| Failure::internal(e.to_string()))
    });

    r.register("systemd.list", 1, |_| -> Outcome {
        let mut units = Vec::with_capacity(MANAGED_UNITS.len());
        for name in MANAGED_UNITS {
            let Ok(status) = svc::get(name) else { continue };
            // A unit that is neither installed nor running is not a service
            // in an unhappy state, it is a service this host does not have:
            // valkey on a box without it, clamd@scan before the scanner is
            // installed. Listing it would be a permanent red row nobody can
            // fix.
            if status.enabled == "not-found" && !status.running() {
                continue;
            }
            units.push(status);
        }
        serde_json::to_value(StatusList { units }).map_err(|e| Failure::internal(e.to_string()))
    });

    r.register("systemd.action", 1, |payload| -> Outcome {
        let req: UnitActionRequest = decode(payload)?;
        match req.action.as_str() {
            "start" => svc::start(&req.unit),
            "stop" => svc::stop(&req.unit),
            "restart" => svc::restart(&req.unit),
            "reload" => svc::reload(&req.unit),
            _ => svc::reload_or_restart(&req.unit),
        }?;
        let status = svc::get(&req.unit)?;
        serde_json::to_value(status).map_err(|e| Failure::internal(e.to_string()))
    });

    r.register("systemd.journal", 1, |payload| -> Outcome {
        let req: JournalRequest = decode(payload)?;
        let text = svc::journal(&req.unit, req.lines)?;
        Ok(json!({ "text": text }))
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_managed_units_pass_validation() {
        assert!(UnitRequest { unit: "mariadb".into() }.validate().is_ok());
        // Well-formed, real, and refused: the allowlist is what stops a flaw
        // anywhere in the panel from reaching systemd-journald or auditd.
        let err = UnitRequest { unit: "systemd-journald".into() }.validate().unwrap_err();
        assert!(err.contains("not managed by opanel"), "{err}");
        let err = UnitRequest { unit: "--now".into() }.validate().unwrap_err();
        assert!(err.contains("not a valid unit name"), "{err}");
    }

    #[test]
    fn verbs_are_the_five_the_panel_needs() {
        for verb in ["start", "stop", "restart", "reload", "reload-or-restart"] {
            let req = UnitActionRequest { unit: "crond".into(), action: verb.into() };
            assert!(req.validate().is_ok(), "{verb} should be accepted");
        }
        let req = UnitActionRequest { unit: "crond".into(), action: "mask".into() };
        assert!(req.validate().is_err(), "mask is not one of the five");
    }
}
