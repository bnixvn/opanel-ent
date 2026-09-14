//! Wrapping dnf.
//!
//! AlmaLinux 10 ships dnf 4.20, not dnf5, so the classic subcommand spelling
//! applies. Phase 0 confirmed this on the target image; if a later EL release
//! switches to dnf5 the only change needed is in this module.

use super::run::{self, Options};
use serde::Serialize;
use std::time::Duration;

const DNF: &str = "dnf";

/// Long operations: a cold metadata refresh plus a large transaction can
/// legitimately take minutes on a small VPS.
const QUERY_TIMEOUT: Duration = Duration::from_secs(3 * 60);
const INSTALL_TIMEOUT: Duration = Duration::from_secs(20 * 60);

/// Reports whether s is an acceptable package name or glob.
///
/// Without this a caller-supplied string starting with "-" would be read by
/// dnf as an option even though it is passed as a separate argv element.
pub fn valid_name(s: &str) -> bool {
    let mut chars = s.chars();
    match chars.next() {
        Some(c) if c.is_ascii_alphanumeric() => {}
        _ => return false,
    }
    if s.len() > 128 {
        return false;
    }
    chars.all(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '.' | '+' | '*' | ':' | '~' | '^' | '-'))
}

fn check_names(names: &[&str]) -> run::Result<()> {
    if names.is_empty() {
        return Err(run::Error::refused("pkgmgr: no package named"));
    }
    for n in names {
        if !valid_name(n) {
            return Err(run::Error::refused(format!("pkgmgr: invalid package name {n:?}")));
        }
    }
    Ok(())
}

/// One installed or available package.
#[derive(Debug, Clone, Serialize)]
pub struct Package {
    pub name: String,
    pub version: String,
    pub repo: String,
}

/// Reports whether every named package is present.
pub fn installed(names: &[&str]) -> run::Result<bool> {
    check_names(names)?;
    // rpm -q exits with the number of packages it could not find, not with 1.
    // Allowing only 1 meant this worked while a single package was missing
    // and failed the install on a machine where none of them were -- which is
    // every machine the installer is actually run on.
    //
    // The range is spelled out rather than allowing anything non-zero, so an
    // exit code that cannot be a count -- a corrupt rpm database, a killed
    // process -- is still reported as the failure it is.
    let missing: Vec<i32> = (1..=names.len() as i32).collect();
    // --whatprovides, because dnf installs by capability and rpm queries by
    // package name, and the two disagree: EL10 ships npm as nodejs-npm, so
    // `dnf install npm` succeeds and `rpm -q npm` then says it is absent.
    // Asking the same question dnf answered means the check agrees with the
    // install instead of re-running it on every pass.
    let mut argv = vec!["rpm", "-q", "--quiet", "--whatprovides"];
    argv.extend_from_slice(names);
    let res = run::cmd_opts(&argv, Options::new().allow_exit(&missing))?;
    Ok(res.exit_code == 0)
}

/// Returns the installed version, or "" when absent.
pub fn installed_version(name: &str) -> run::Result<String> {
    check_names(&[name])?;
    let res = run::cmd_opts(
        &["rpm", "-q", "--qf", "%{VERSION}-%{RELEASE}", name],
        Options::new().allow_exit(&[1]),
    )?;
    if res.exit_code != 0 {
        return Ok(String::new());
    }
    Ok(res.stdout.trim().to_owned())
}

/// Adds packages. Idempotent: already-present packages are a no-op for dnf.
pub fn install(names: &[&str]) -> run::Result<()> {
    check_names(names)?;
    let mut argv = vec![DNF, "install", "-y", "--setopt=install_weak_deps=False"];
    argv.extend_from_slice(names);
    run::cmd_opts(&argv, Options::new().timeout(INSTALL_TIMEOUT))?;
    Ok(())
}

/// Deletes packages.
pub fn remove(names: &[&str]) -> run::Result<()> {
    check_names(names)?;
    let mut argv = vec![DNF, "remove", "-y"];
    argv.extend_from_slice(names);
    run::cmd_opts(&argv, Options::new().timeout(INSTALL_TIMEOUT))?;
    Ok(())
}

/// Lists packages matching a glob that can be installed.
pub fn available(glob: &str) -> run::Result<Vec<Package>> {
    check_names(&[glob])?;
    // Exit 1 means "nothing matched", which is an empty list rather than an
    // error the caller should have to distinguish.
    let res = run::cmd_opts(
        &[DNF, "-q", "list", "--available", glob],
        Options::new().timeout(QUERY_TIMEOUT).allow_exit(&[1]),
    )?;
    if res.exit_code == 1 {
        return Ok(Vec::new());
    }
    Ok(parse_list(&res.stdout))
}

/// Lists packages with a pending update. dnf check-update exits 100 when
/// updates exist and 0 when none do, so both are success here.
pub fn upgradable() -> run::Result<Vec<Package>> {
    check_update(&[DNF, "-q", "check-update"])
}

/// Lists only packages with a security advisory.
pub fn security_upgradable() -> run::Result<Vec<Package>> {
    check_update(&[DNF, "-q", "check-update", "--security"])
}

fn check_update(argv: &[&str]) -> run::Result<Vec<Package>> {
    let res = run::cmd_opts(argv, Options::new().timeout(QUERY_TIMEOUT).allow_exit(&[100]))?;
    if res.exit_code == 0 {
        return Ok(Vec::new());
    }
    Ok(parse_list(&res.stdout))
}

/// Applies every pending update.
pub fn upgrade_all(security_only: bool) -> run::Result<()> {
    let mut argv = vec![DNF, "upgrade", "-y"];
    if security_only {
        argv.push("--security");
    }
    run::cmd_opts(&argv, Options::new().timeout(INSTALL_TIMEOUT))?;
    Ok(())
}

/// Reads the three-column output shared by "dnf list" and "dnf check-update":
/// name.arch, version, repo. Header lines and the "Obsoleting Packages"
/// trailer have a different shape and are skipped.
fn parse_list(out: &str) -> Vec<Package> {
    let mut pkgs = Vec::new();
    for line in out.lines() {
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.len() != 3 {
            continue;
        }
        let name = fields[0];
        let Some(dot) = name.rfind('.') else {
            continue; // not "name.arch"; a header or wrapped line
        };
        if name.ends_with(':') || name.eq_ignore_ascii_case("Last") {
            continue;
        }
        pkgs.push(Package {
            name: name[..dot].to_owned(),
            version: fields[1].to_owned(),
            repo: fields[2].to_owned(),
        });
    }
    pkgs
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn package_names_are_checked_by_shape() {
        for good in ["nftables", "alt-php74*", "nodejs:22", "libstdc++", "a"] {
            assert!(valid_name(good), "{good} should be accepted");
        }
        for bad in ["--assumeyes", "-y", "two words", "a;b", "", "/etc/passwd"] {
            assert!(!valid_name(bad), "{bad:?} should be refused");
        }
    }

    #[test]
    fn list_output_skips_everything_that_is_not_a_package() {
        let out = concat!(
            "Last metadata expiration check: 0:03:21 ago on Mon 15 Sep 2025.\n",
            "Available Packages\n",
            "nano.x86_64                 8.1-1.el10               appstream\n",
            "nano-default-editor.noarch  8.1-1.el10               appstream\n",
            "\n",
            "Obsoleting Packages\n",
        );
        let pkgs = parse_list(out);
        assert_eq!(pkgs.len(), 2, "got {pkgs:?}");
        assert_eq!(pkgs[0].name, "nano");
        assert_eq!(pkgs[0].version, "8.1-1.el10");
        assert_eq!(pkgs[0].repo, "appstream");
        // The arch is stripped from the last dot only: a name with dots of
        // its own keeps them.
        assert_eq!(parse_list("python3.12.x86_64 3.12.9-1 appstream\n")[0].name, "python3.12");
    }
}
