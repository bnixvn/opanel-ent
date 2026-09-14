//! Identity and reachability: what this binary is, and what it serves.

use crate::registry::{Failure, Outcome, Registry};
use serde::Serialize;
use serde_json::json;
use std::collections::BTreeMap;
use std::sync::OnceLock;

/// Set once at startup so `agent.actions` can report the table without the
/// handler holding a reference to the registry that owns it.
static ACTION_TABLE: OnceLock<BTreeMap<String, i64>> = OnceLock::new();

pub fn publish_table(names: BTreeMap<String, i64>) {
    let _ = ACTION_TABLE.set(names);
}

#[derive(Serialize)]
struct Ping {
    agent: &'static str,
    version: String,
    pid: u32,
}

/// What /etc/os-release says, plus whether CloudLinux is underneath it.
#[derive(Serialize, Default)]
pub struct Distro {
    pub id: String,
    pub version_id: String,
    pub major: i64,
    pub pretty: String,
    pub cloudlinux: bool,
}

#[derive(Serialize)]
struct SysInfo {
    distro: Distro,
    #[serde(skip_serializing_if = "String::is_empty")]
    uptime: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    webserver: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    php_provider: String,
}

pub fn register(r: &mut Registry) {
    r.register("ping", 1, |_| {
        Ok(serde_json::to_value(Ping {
            agent: "opanel-agent",
            version: crate::version::string(),
            pid: std::process::id(),
        })
        .map_err(|e| Failure::internal(e.to_string()))?)
    });

    r.register("sysinfo", 1, |_| -> Outcome {
        Ok(serde_json::to_value(SysInfo {
            distro: detect_distro(),
            uptime: host_uptime(),
            webserver: "ols".to_owned(),
            php_provider: "lsphp".to_owned(),
        })
        .map_err(|e| Failure::internal(e.to_string()))?)
    });

    r.register("agent.actions", 1, |_| {
        Ok(json!(ACTION_TABLE.get().cloned().unwrap_or_default()))
    });
}

/// Reads /etc/os-release.
///
/// CloudLinux is detected by its own release file rather than by os-release,
/// which still says AlmaLinux after a conversion -- the ID does not change,
/// so anything keying off it would decide wrongly.
pub fn detect_distro() -> Distro {
    let mut d = Distro::default();
    let Ok(text) = std::fs::read_to_string("/etc/os-release") else {
        return d;
    };
    for line in text.lines() {
        let Some((key, value)) = line.split_once('=') else { continue };
        let value = value.trim_matches('"').to_owned();
        match key {
            "ID" => d.id = value,
            "VERSION_ID" => d.version_id = value,
            "PRETTY_NAME" => d.pretty = value,
            _ => {}
        }
    }
    d.major = d
        .version_id
        .split('.')
        .next()
        .and_then(|s| s.parse().ok())
        .unwrap_or(0);
    d.cloudlinux = std::path::Path::new("/etc/cloudlinux-release").exists();
    d
}

/// How long the machine has been up, in a form meant to be read rather than
/// parsed.
fn host_uptime() -> String {
    let seconds = crate::actions::sysstat::uptime_seconds();
    if seconds <= 0 {
        return String::new();
    }
    let (d, h, m) = (seconds / 86400, (seconds % 86400) / 3600, (seconds % 3600) / 60);
    if d > 0 {
        format!("{d}d {h}h")
    } else {
        format!("{h}h {m}m")
    }
}
