package phpmgr

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/bnixvn/opanel-ent/internal/platform/pkgmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/svc"
)

// AltPHPRoot is where CloudLinux installs each version: a complete tree per
// version, the same shape Remi uses and for the same reason.
const AltPHPRoot = "/opt/alt"

// altPHPVersions is what CloudLinux publishes for el10, read from the host
// rather than from documentation -- fifteen of them, back to 5.3.
//
// That reach is the whole argument for this provider over Remi's. Remi stops
// at 7.4 and ships it unpatched; CloudLinux backports security fixes into
// every one of these under the name HardenedPHP, which is what makes hosting
// a customer's 5.6 application a defensible thing to do rather than merely a
// possible one.
var altPHPVersions = []string{
	"5.3", "5.4", "5.5", "5.6",
	"7.0", "7.1", "7.2", "7.3", "7.4",
	"8.0", "8.1", "8.2", "8.3", "8.4", "8.5",
}

// AltPHP runs sites on CloudLinux's PHP.
//
// Pools, like Remi, and deliberately so. CloudLinux's own way of serving PHP
// under Apache is mod_lsapi, which takes the version from the PHP Selector --
// and the Selector's unit is the Linux account, not the website. Measured on
// a host: a vhost asking for 8.4 through mod_lsapi ran 8.1, because 8.1 was
// what the selector had for that uid, and setting a different one needs the
// account inside CageFS. A panel whose customers choose PHP per site cannot
// express that, so the interpreter stays pool-shaped and the version stays a
// property of the site.
//
// What changes against Remi is the build, and that is the point: LiteSpeed
// Enterprise can be handed an alt-php and cannot be handed a Remi one. With
// both servers on /opt/alt, a failover between them does not change which
// extensions a site has or which php.ini it reads.
//
// PHP Selector still earns its place -- it is what a customer points their
// own shell and cron at, which is what it was built for.
type AltPHP struct{ root string }

// NewAltPHP returns the CloudLinux PHP provider.
func NewAltPHP() *AltPHP { return &AltPHP{root: AltPHPRoot} }

// Name identifies the provider.
func (p *AltPHP) Name() string { return ProviderAltPHP }

// Supported lists the versions CloudLinux publishes.
func (p *AltPHP) Supported() []string { return append([]string(nil), altPHPVersions...) }

// The path methods below name files on the host the agent runs on, which is
// always Linux, so they use path.Join rather than filepath.Join. filepath
// uses the separator of whichever machine compiled the code: on a developer's
// Windows box every one of these came out with backslashes, which is correct
// for a local file and meaningless as a php-fpm socket. Remi's provider had
// the same latent bug and it is fixed there too.

// prefix is the tree for one version: /opt/alt/php84.
func (p *AltPHP) prefix(version string) string {
	return path.Join(p.root, "php"+pkgSuffix(version))
}

// CLIBinary is the command-line interpreter, for WP-CLI, cron and the
// terminal.
func (p *AltPHP) CLIBinary(version string) string {
	return path.Join(p.prefix(version), "usr", "bin", "php")
}

// LSPHPBinary is the LSAPI interpreter. Not part of the Provider interface:
// only LiteSpeed spawns it, and it is here so the agent can ask whether the
// other server would have something to run this version with.
func (p *AltPHP) LSPHPBinary(version string) string {
	return path.Join(p.prefix(version), "usr", "bin", "lsphp")
}

// FPMBinary is the pool manager.
func (p *AltPHP) FPMBinary(version string) string {
	return path.Join(p.prefix(version), "usr", "sbin", "php-fpm")
}

// IniDropIn is the panel's override file in this version's scan directory.
//
// Written to etc/php.d rather than through link/conf, which is a symlink to
// it. The symlink belongs to the PHP Selector, which rebuilds that farm on
// every --setup-cl-selector; writing to the real directory means the panel's
// file cannot be a casualty of a selector refresh.
func (p *AltPHP) IniDropIn(version string) string {
	return path.Join(p.prefix(version), "etc", "php.d", "99-opanel.ini")
}

// IonCubeLoader is the loader, which CloudLinux packages for every version
// rather than leaving to be installed by hand.
func (p *AltPHP) IonCubeLoader(version string) string {
	return path.Join(p.prefix(version), "usr", "lib64", "php", "modules", "ioncube_loader.so")
}

// PoolDir is where this version's pool definitions live. The packaged
// www.conf sits here too, which is why pool files carry a prefix.
func (p *AltPHP) PoolDir(version string) string {
	return path.Join(p.prefix(version), "etc", "php-fpm.d")
}

// PoolFile is the panel's definition for one pool.
func (p *AltPHP) PoolFile(version, pool string) string {
	return path.Join(p.PoolDir(version), "opanel-"+pool+".conf")
}

// SocketPath is the unix socket a pool listens on.
//
// The same directory Remi's pools use, and the same naming: a host being
// moved from one provider to the other rewrites the vhosts anyway, and one
// socket directory is one tmpfiles rule and one set of permissions to get
// right.
func (p *AltPHP) SocketPath(version, pool string) string {
	return path.Join(FPMSocketDir, pool+"-alt"+pkgSuffix(version)+".sock")
}

// ServiceUnit is the systemd unit running this version's pools.
func (p *AltPHP) ServiceUnit(version string) string {
	return "alt-php" + pkgSuffix(version) + "-fpm"
}

// Available reports which supported versions are installed, how many sites
// each runs, and whether its pool manager is up.
func (p *AltPHP) Available(ctx context.Context) ([]Version, error) {
	out := make([]Version, 0, len(altPHPVersions))
	for _, v := range altPHPVersions {
		ver := Version{Version: v}
		// The binary, not the directory. Installing part of an alt-php group
		// leaves /opt/alt/phpNN behind with no interpreter in it; this host
		// had thirteen such directories and three usable versions.
		if st, err := os.Stat(p.FPMBinary(v)); err == nil && !st.IsDir() {
			ver.Installed = true
			ver.CLIPath = p.CLIBinary(v)
			ver.FPMPath = p.FPMBinary(v)
			ver.Pools = p.PoolCount(v)
			if st, err := svc.Get(ctx, p.ServiceUnit(v)); err == nil {
				ver.Running = st.Running()
			}
			if st, err := os.Stat(p.IonCubeLoader(v)); err == nil && !st.IsDir() {
				ver.IonCube = true
			}
		}
		out = append(out, ver)
	}
	return out, nil
}

// PoolCount is how many sites run on this version, counted from the files on
// disk rather than from the database: this is the agent's side, and what the
// interpreter will serve is what was written.
func (p *AltPHP) PoolCount(version string) int {
	matches, err := filepath.Glob(filepath.Join(p.PoolDir(version), "opanel-*.conf"))
	if err != nil {
		return 0
	}
	return len(matches)
}

// Install adds a version as the package group CloudLinux publishes it as.
//
// A group rather than a list of packages the panel curates: the group is
// alt-phpNN and it carries the extension set CloudLinux considers complete
// for that version, which on 8.4 is about eighty packages including ionCube
// and a PECL collection this panel has no business choosing from.
func (p *AltPHP) Install(ctx context.Context, version string) error {
	if err := checkVersion(p, version); err != nil {
		return err
	}
	group := "alt-php" + pkgSuffix(version)
	if err := pkgmgr.InstallGroup(ctx, group); err != nil {
		return fmt.Errorf("install %s: %w", group, err)
	}
	// The pool manager is not in every version's group -- it is its own
	// package -- and without it the version cannot run a site.
	if err := pkgmgr.Install(ctx, group+"-php-fpm"); err != nil {
		return fmt.Errorf("install %s-php-fpm: %w", group, err)
	}
	return nil
}

// Uninstall removes a version and everything installed with it.
func (p *AltPHP) Uninstall(ctx context.Context, version string) error {
	if err := checkVersion(p, version); err != nil {
		return err
	}
	return pkgmgr.Remove(ctx, "alt-php"+pkgSuffix(version)+"*")
}

// AltPHPInstalled reports whether the host has CloudLinux's PHP at all.
//
// Used to decide which provider a host runs, so it asks about an interpreter
// rather than about a directory or a package name: a partial group leaves
// both behind.
func AltPHPInstalled() bool {
	p := NewAltPHP()
	for _, v := range altPHPVersions {
		if st, err := os.Stat(p.FPMBinary(v)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}
