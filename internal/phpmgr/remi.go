package phpmgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/platform/pkgmgr"
)

// RemiRoot is where Remi's packages install each version. The layout is the
// software-collection one: every version is a complete tree under its own
// prefix, which is what lets seven of them coexist.
const RemiRoot = "/opt/remi"

// RemiRepoRPM defines the repository. Remi ships remi-safe enabled and the
// versioned repositories disabled, so installing this changes nothing about
// what dnf would do to the rest of the system until a version is asked for.
const RemiRepoRPM = "https://rpms.remirepo.net/enterprise/remi-release-10.rpm"

// remiVersions is what the repository publishes for el10, confirmed against
// it rather than assumed.
//
// 7.4 is the reason this provider exists. It has been end of life since 2022
// and no sensible person starts a new project on it, but a panel that cannot
// host the applications people actually have is a panel nobody can migrate
// to. lsphp does not publish it for el10; Remi does.
//
// 8.6 is published as a development branch and deliberately left out: a
// version that changes under a running site is not something to offer.
var remiVersions = []string{"7.4", "8.0", "8.1", "8.2", "8.3", "8.4", "8.5"}

// remiExtensions is installed alongside each version. Names are package
// suffixes; what exists differs by version, so Install filters this list
// against the repository instead of failing on the first gap.
var remiExtensions = []string{
	"mysqlnd", "opcache", "intl", "gd", "mbstring", "bcmath", "xml",
	"pdo", "process", "soap", "zip", "sodium",
	"pecl-redis", "pecl-imagick", "pecl-apcu", "pecl-igbinary",
}

// FPMSocketDir holds one socket per pool. On tmpfs, so a stale socket cannot
// outlive across a reboot the process that made it.
//
// Deliberately not under /run/opanel. That directory is the agent's own, mode
// 0750 owned root:opanel, and the webserver is not in that group -- so a
// socket inside it is unreachable however correct the socket's own mode is.
// The failure looks exactly like a permissions bug on the socket, which is
// the one place it is not. The terminal's runtime directory was moved out for
// the same reason.
const FPMSocketDir = "/run/opanel-fpm"

// pkgSuffix turns "8.4" into "84", the form the package names use.
func pkgSuffix(version string) string { return strings.ReplaceAll(version, ".", "") }

// Remi installs PHP-FPM from the Remi repository.
//
// The interpreter runs as a pool of its own processes owned by the site's
// account, and the webserver proxies to a unix socket. That is a different
// shape from LiteSpeed's, which spawns the interpreter itself -- see
// FPMProvider for the part of the interface that only makes sense here.
type Remi struct{ root string }

// NewRemi returns the PHP-FPM provider.
func NewRemi() *Remi { return &Remi{root: RemiRoot} }

// Name identifies the provider.
func (p *Remi) Name() string { return ProviderRemi }

// Supported lists the versions available for el10.
func (p *Remi) Supported() []string { return append([]string(nil), remiVersions...) }

// prefix is the collection directory for a version: /opt/remi/php83.
func (p *Remi) prefix(version string) string {
	return filepath.Join(p.root, "php"+pkgSuffix(version))
}

// CLIBinary is the command-line interpreter for this version.
func (p *Remi) CLIBinary(version string) string {
	return filepath.Join(p.prefix(version), "root", "usr", "bin", "php")
}

// FPMBinary is the pool manager itself.
func (p *Remi) FPMBinary(version string) string {
	return filepath.Join(p.prefix(version), "root", "usr", "sbin", "php-fpm")
}

// IniDropIn is the panel's override file inside the version's scan directory.
func (p *Remi) IniDropIn(version string) string {
	return filepath.Join("/etc/opt/remi", "php"+pkgSuffix(version), "php.d", "99-opanel.ini")
}

// IonCubeLoader is where the loader would be. Remi does not package ionCube;
// the path is where a manually installed loader belongs, and Available
// reports whether one is actually there.
func (p *Remi) IonCubeLoader(version string) string {
	return filepath.Join(p.prefix(version), "root", "usr", "lib64", "php", "modules", "ioncube_loader.so")
}

// PoolDir is the directory this version's pool definitions live in.
func (p *Remi) PoolDir(version string) string {
	return filepath.Join("/etc/opt/remi", "php"+pkgSuffix(version), "php-fpm.d")
}

// PoolFile is the panel's definition for one pool. The opanel- prefix is what
// makes pruning safe: the packaged www.conf sits in the same directory and
// must survive.
func (p *Remi) PoolFile(version, pool string) string {
	return filepath.Join(p.PoolDir(version), "opanel-"+pool+".conf")
}

// SocketPath is the unix socket a pool listens on.
func (p *Remi) SocketPath(version, pool string) string {
	return filepath.Join(FPMSocketDir, pool+"-"+pkgSuffix(version)+".sock")
}

// ServiceUnit is the systemd unit running this version's pools.
func (p *Remi) ServiceUnit(version string) string {
	return "php" + pkgSuffix(version) + "-php-fpm"
}

// Available reports which supported versions are installed.
func (p *Remi) Available(ctx context.Context) ([]Version, error) {
	_ = ctx
	out := make([]Version, 0, len(remiVersions))
	for _, v := range remiVersions {
		ver := Version{Version: v}
		if st, err := os.Stat(p.FPMBinary(v)); err == nil && !st.IsDir() {
			ver.Installed = true
			ver.CLIPath = p.CLIBinary(v)
			ver.FPMPath = p.FPMBinary(v)
			if st, err := os.Stat(p.IonCubeLoader(v)); err == nil && !st.IsDir() {
				ver.IonCube = true
			}
		}
		out = append(out, ver)
	}
	return out, nil
}

// Install adds a version with as much of the standard extension set as the
// repository carries for it.
func (p *Remi) Install(ctx context.Context, version string) error {
	if err := checkVersion(p, version); err != nil {
		return err
	}
	if err := p.ensureRepo(ctx); err != nil {
		return err
	}
	base := "php" + pkgSuffix(version) + "-php"

	// The pool manager first: without it the version is not something a site
	// can be pointed at, and a missing extension is not worth failing on.
	if err := pkgmgr.Install(ctx, base+"-fpm", base+"-cli"); err != nil {
		return fmt.Errorf("install php %s: %w", version, err)
	}

	wanted := make([]string, 0, len(remiExtensions))
	for _, ext := range remiExtensions {
		name := base + "-" + ext
		avail, err := pkgmgr.Available(ctx, name)
		if err != nil || len(avail) == 0 {
			continue // not published for this version
		}
		wanted = append(wanted, name)
	}
	if len(wanted) > 0 {
		if err := pkgmgr.Install(ctx, wanted...); err != nil {
			return fmt.Errorf("install php %s extensions: %w", version, err)
		}
	}
	return nil
}

// Uninstall removes a version and everything installed with it.
func (p *Remi) Uninstall(ctx context.Context, version string) error {
	if err := checkVersion(p, version); err != nil {
		return err
	}
	return pkgmgr.Remove(ctx, "php"+pkgSuffix(version)+"-php-*")
}

// ensureRepo installs the repository definition if it is not already there.
func (p *Remi) ensureRepo(ctx context.Context) error {
	ok, err := pkgmgr.Installed(ctx, "remi-release")
	if err == nil && ok {
		return nil
	}
	if err := pkgmgr.InstallURL(ctx, RemiRepoRPM); err != nil {
		return fmt.Errorf("install the Remi repository: %w", err)
	}
	return nil
}
