package phpmgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/platform/pkgmgr"
)

// LSPHPRoot is where the LiteSpeed packages install each version.
const LSPHPRoot = "/usr/local/lsws"

// lsphpVersions is what the LiteSpeed repository actually ships for el10,
// confirmed against the repository in Phase 0. There is deliberately no 7.4:
// it is not published for el10, and pretending otherwise would produce an
// install action that always fails. Sites needing PHP 7.4 or older are the
// reason Stage B moves to CloudLinux alt-php.
var lsphpVersions = []string{"8.1", "8.2", "8.3", "8.4", "8.5"}

// lsphpExtensions is installed alongside each version. Every name here is a
// package suffix; availability differs between versions, so the installer
// filters the list against the repository rather than failing on the first
// version that lacks one.
var lsphpExtensions = []string{
	"common", "mysqlnd", "opcache", "intl", "gd", "mbstring",
	"bcmath", "pdo", "process", "ioncube",
	"pecl-redis", "pecl-imagick", "pecl-apcu", "pecl-igbinary",
}

// LSPHP installs PHP from the LiteSpeed repository.
type LSPHP struct{ root string }

// NewLSPHP returns the Stage A provider.
func NewLSPHP() *LSPHP { return &LSPHP{root: LSPHPRoot} }

// Name identifies the provider.
func (p *LSPHP) Name() string { return "lsphp" }

// Supported lists the versions available for el10.
func (p *LSPHP) Supported() []string { return append([]string(nil), lsphpVersions...) }

// pkgSuffix turns "8.4" into "84", the form the package names use.
func pkgSuffix(version string) string { return strings.ReplaceAll(version, ".", "") }

func (p *LSPHP) dir(version string) string {
	return filepath.Join(p.root, "lsphp"+pkgSuffix(version))
}

// LSAPIBinary is the binary a vhost's external processor runs.
func (p *LSPHP) LSAPIBinary(version string) string {
	return filepath.Join(p.dir(version), "bin", "lsphp")
}

// CLIBinary is the command-line interpreter for this version.
func (p *LSPHP) CLIBinary(version string) string {
	return filepath.Join(p.dir(version), "bin", "php")
}

// IniDropIn is the panel's override file inside the version's scan directory.
//
// The path was confirmed on the target host in Phase 0: LiteSpeed's el10
// packages use etc/php.d, not the Debian-style etc/php/<ver>/mods-available
// layout that v1 wrote to.
func (p *LSPHP) IniDropIn(version string) string {
	return filepath.Join(p.dir(version), "etc", "php.d", "99-opanel.ini")
}

// Available reports which supported versions are installed.
func (p *LSPHP) Available(ctx context.Context) ([]Version, error) {
	out := make([]Version, 0, len(lsphpVersions))
	for _, v := range lsphpVersions {
		bin := p.LSAPIBinary(v)
		installed := false
		if st, err := os.Stat(bin); err == nil && !st.IsDir() {
			installed = true
		}
		ver := Version{Version: v, Installed: installed}
		if installed {
			ver.LSAPIPath = bin
			ver.CLIPath = p.CLIBinary(v)
		}
		out = append(out, ver)
	}
	_ = ctx
	return out, nil
}

// Install adds a version and as much of the standard extension set as the
// repository carries for it.
func (p *LSPHP) Install(ctx context.Context, version string) error {
	if err := checkVersion(p, version); err != nil {
		return err
	}
	base := "lsphp" + pkgSuffix(version)

	// Install the interpreter first: a missing base package is a hard error,
	// whereas a missing extension is not worth failing the whole operation.
	if err := pkgmgr.Install(ctx, base); err != nil {
		return fmt.Errorf("install %s: %w", base, err)
	}

	wanted := make([]string, 0, len(lsphpExtensions))
	for _, ext := range lsphpExtensions {
		name := base + "-" + ext
		avail, err := pkgmgr.Available(ctx, name)
		if err != nil || len(avail) == 0 {
			continue // not published for this version
		}
		wanted = append(wanted, name)
	}
	if len(wanted) > 0 {
		if err := pkgmgr.Install(ctx, wanted...); err != nil {
			return fmt.Errorf("install extensions for %s: %w", base, err)
		}
	}
	return nil
}

// Uninstall removes a version and its extensions.
func (p *LSPHP) Uninstall(ctx context.Context, version string) error {
	if err := checkVersion(p, version); err != nil {
		return err
	}
	base := "lsphp" + pkgSuffix(version)
	installed, err := pkgmgr.Installed(ctx, base)
	if err != nil {
		return err
	}
	if !installed {
		return nil
	}
	// The glob catches the extension subpackages; dnf resolves it against
	// installed packages, so a version with no extensions still removes.
	return pkgmgr.Remove(ctx, base, base+"-*")
}
