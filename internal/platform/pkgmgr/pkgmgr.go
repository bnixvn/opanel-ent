// Package pkgmgr wraps dnf.
//
// AlmaLinux 10 ships dnf 4.20, not dnf5, so the classic subcommand spelling
// applies. Phase 0 confirmed this on the target image; if a later EL release
// switches to dnf5 the only change needed is in this package.
package pkgmgr

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

const dnf = "dnf"

// Long operations: a cold metadata refresh plus a large transaction can
// legitimately take minutes on a small VPS.
const (
	queryTimeout   = 3 * time.Minute
	installTimeout = 20 * time.Minute
)

// namePattern rejects anything that is not a plausible package name or glob.
// Without it a caller-supplied string starting with "-" would be read by dnf
// as an option even though it is passed as a separate argv element.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+*:~^-]{0,127}$`)

// ValidName reports whether s is an acceptable package name or glob.
func ValidName(s string) bool { return namePattern.MatchString(s) }

func checkNames(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("pkgmgr: no package named")
	}
	for _, n := range names {
		if !ValidName(n) {
			return fmt.Errorf("pkgmgr: invalid package name %q", n)
		}
	}
	return nil
}

// Package is one installed or available package.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Repo    string `json:"repo"`
}

// Installed reports whether every named package is present.
func Installed(ctx context.Context, names ...string) (bool, error) {
	if err := checkNames(names); err != nil {
		return false, err
	}
	// rpm -q exits with the number of packages it could not find, not with 1.
	// Allowing only 1 meant this worked while a single package was missing
	// and failed the install on a machine where none of them were -- which
	// is every machine the installer is actually run on.
	//
	// The range is spelled out rather than allowing anything non-zero, so an
	// exit code that cannot be a count -- a corrupt rpm database, a killed
	// process -- is still reported as the failure it is.
	missing := make([]int, len(names))
	for i := range missing {
		missing[i] = i + 1
	}
	// --whatprovides, because dnf installs by capability and rpm queries by
	// package name, and the two disagree: EL10 ships npm as nodejs-npm, so
	// `dnf install npm` succeeds and `rpm -q npm` then says it is absent.
	// Asking the same question dnf answered means the check agrees with the
	// install instead of re-running it on every pass.
	res, err := run.Cmd(ctx, append([]string{"rpm", "-q", "--quiet", "--whatprovides"}, names...),
		run.AllowExit(missing...))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// InstalledVersion returns the installed version, or "" when absent.
func InstalledVersion(ctx context.Context, name string) (string, error) {
	if err := checkNames([]string{name}); err != nil {
		return "", err
	}
	res, err := run.Cmd(ctx, []string{"rpm", "-q", "--qf", "%{VERSION}-%{RELEASE}", name}, run.AllowExit(1))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Install adds packages. It is idempotent: already-present packages are a
// no-op for dnf.
func Install(ctx context.Context, names ...string) error {
	if err := checkNames(names); err != nil {
		return err
	}
	argv := append([]string{dnf, "install", "-y", "--setopt=install_weak_deps=False"}, names...)
	_, err := run.Cmd(ctx, argv, run.Timeout(installTimeout))
	return err
}

// urlPattern accepts an https URL to an rpm and nothing else.
//
// Installing by URL is how a repository definition arrives, and it is the one
// place dnf is handed something that is not a package name. Restricting it to
// https and to a .rpm suffix keeps that hole the shape of the thing it exists
// for: no http, no file://, no path that could be read as an option.
var urlPattern = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(?::[0-9]{1,5})?/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*\.rpm$`)

// InstallURL adds a package from an https URL, for repository definitions.
func InstallURL(ctx context.Context, url string) error {
	if !urlPattern.MatchString(url) {
		return fmt.Errorf("pkgmgr: %q is not an https url to an rpm", url)
	}
	_, err := run.Cmd(ctx, []string{dnf, "install", "-y", url}, run.Timeout(installTimeout))
	return err
}

// Remove deletes packages.
func Remove(ctx context.Context, names ...string) error {
	if err := checkNames(names); err != nil {
		return err
	}
	_, err := run.Cmd(ctx, append([]string{dnf, "remove", "-y"}, names...), run.Timeout(installTimeout))
	return err
}

// Available lists packages matching a glob that can be installed.
func Available(ctx context.Context, glob string) ([]Package, error) {
	if err := checkNames([]string{glob}); err != nil {
		return nil, err
	}
	// Exit 1 means "nothing matched", which is an empty list rather than an
	// error the caller should have to distinguish.
	res, err := run.Cmd(ctx, []string{dnf, "-q", "list", "--available", glob},
		run.Timeout(queryTimeout), run.AllowExit(1))
	if err != nil {
		return nil, err
	}
	if res.ExitCode == 1 {
		return nil, nil
	}
	return parseList(res.Stdout), nil
}

// Upgradable lists packages with a pending update. dnf check-update exits 100
// when updates exist and 0 when none do, so both are success here.
func Upgradable(ctx context.Context) ([]Package, error) {
	res, err := run.Cmd(ctx, []string{dnf, "-q", "check-update"},
		run.Timeout(queryTimeout), run.AllowExit(100))
	if err != nil {
		return nil, err
	}
	if res.ExitCode == 0 {
		return nil, nil
	}
	return parseList(res.Stdout), nil
}

// SecurityUpgradable lists only packages with a security advisory.
func SecurityUpgradable(ctx context.Context) ([]Package, error) {
	res, err := run.Cmd(ctx, []string{dnf, "-q", "check-update", "--security"},
		run.Timeout(queryTimeout), run.AllowExit(100))
	if err != nil {
		return nil, err
	}
	if res.ExitCode == 0 {
		return nil, nil
	}
	return parseList(res.Stdout), nil
}

// UpgradeAll applies every pending update.
func UpgradeAll(ctx context.Context, securityOnly bool) error {
	argv := []string{dnf, "upgrade", "-y"}
	if securityOnly {
		argv = append(argv, "--security")
	}
	_, err := run.Cmd(ctx, argv, run.Timeout(installTimeout))
	return err
}

// parseList reads the three-column output shared by "dnf list" and
// "dnf check-update": name.arch, version, repo. Header lines and the
// "Obsoleting Packages" trailer have a different shape and are skipped.
func parseList(out string) []Package {
	var pkgs []Package
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		name := fields[0]
		if !strings.Contains(name, ".") {
			continue // not "name.arch"; a header or wrapped line
		}
		if strings.HasSuffix(name, ":") || strings.EqualFold(name, "Last") {
			continue
		}
		pkgs = append(pkgs, Package{
			Name:    name[:strings.LastIndex(name, ".")],
			Version: fields[1],
			Repo:    fields[2],
		})
	}
	return pkgs
}
