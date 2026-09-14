//! Asking dnf what is installed, and applying updates.

use crate::platform::pkgmgr::{self, Package};
use crate::registry::{decode, Failure, Outcome, Registry, Validate};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

/// Names a package or glob to look up.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PackageQuery {
    pub name: String,
}

impl Validate for PackageQuery {
    fn validate(&self) -> Result<(), String> {
        if !pkgmgr::valid_name(&self.name) {
            return Err(format!("package name {:?} is not valid", self.name));
        }
        Ok(())
    }
}

/// Reports whether a package is installed.
#[derive(Serialize)]
struct PackageStatus {
    name: String,
    installed: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    version: String,
}

/// Asks for pending updates to be applied.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct UpgradeRequest {
    #[serde(default)]
    pub security_only: bool,
}

impl Validate for UpgradeRequest {
    fn validate(&self) -> Result<(), String> {
        Ok(())
    }
}

/// An input that carries nothing, so an unexpected field is still refused
/// rather than ignored.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NoInput {}

impl Validate for NoInput {
    fn validate(&self) -> Result<(), String> {
        Ok(())
    }
}

pub fn register(r: &mut Registry) {
    r.register("pkg.status", 1, |payload| -> Outcome {
        let q: PackageQuery = decode(payload)?;
        let version = pkgmgr::installed_version(&q.name)?;
        serde_json::to_value(PackageStatus {
            name: q.name,
            installed: !version.is_empty(),
            version,
        })
        .map_err(|e| Failure::internal(e.to_string()))
    });

    r.register("pkg.available", 1, |payload| -> Outcome {
        let q: PackageQuery = decode(payload)?;
        Ok(package_list(pkgmgr::available(&q.name)?))
    });

    r.register("pkg.upgradable", 1, |payload| -> Outcome {
        let _: NoInput = decode(payload)?;
        Ok(package_list(pkgmgr::upgradable()?))
    });

    r.register("pkg.upgradable_security", 1, |payload| -> Outcome {
        let _: NoInput = decode(payload)?;
        Ok(package_list(pkgmgr::security_upgradable()?))
    });

    // No pkg.install action. Installing arbitrary packages is a far larger
    // grant than reading their status, and nothing needs it: the narrowly
    // scoped actions (php.install, and so on) name their own allowlists
    // instead.
    r.register_slow(
        "pkg.upgrade_all",
        1,
        // A full dnf transaction on a small VPS outlasts the default budget
        // several times over, and a half-applied upgrade is worse than a
        // slow one.
        std::time::Duration::from_secs(25 * 60),
        |payload| -> Outcome {
            let req: UpgradeRequest = decode(payload)?;
            pkgmgr::upgrade_all(req.security_only)?;
            Ok(json!({}))
        },
    );
}

/// Wraps a result set.
///
/// An empty list is sent as null, not `[]`, because that is what the Go
/// agent's nil slice encodes to and the panel is reading both agents during
/// the port. The panel treats the two the same; the wire does not have to
/// change twice.
fn package_list(pkgs: Vec<Package>) -> Value {
    if pkgs.is_empty() {
        return json!({ "packages": Value::Null });
    }
    json!({ "packages": pkgs })
}
