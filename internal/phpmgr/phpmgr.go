// Package phpmgr abstracts where PHP comes from.
//
// One provider today: Remi's PHP-FPM packages, which carry 7.4 through 8.5
// for EL10. CloudLinux alt-php is the one that would join it, for HardenedPHP
// on versions past end of life.
//
// The abstraction earns its place at exactly one point: a site stores the
// string "8.4", never a binary path. Changing provider is then a re-render of
// every vhost rather than a data migration.
package phpmgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// versionPattern accepts "8.4" and rejects anything that could become a path
// component or a command-line flag.
var versionPattern = regexp.MustCompile(`^[5-9]\.[0-9]$`)

// ValidVersion reports whether v has the shape of a PHP version.
func ValidVersion(v string) bool { return versionPattern.MatchString(v) }

// Version describes one PHP version on the host.
type Version struct {
	Version   string `json:"version"` // "8.4"
	Installed bool   `json:"installed"`
	FPMPath   string `json:"fpm_path,omitempty"` // pool manager the server proxies to
	// Pools is how many sites run on this version, and Running whether its
	// pool manager is up.
	//
	// Both are reported because the pair is the answer to a question the
	// panel was getting asked: a version with no sites is deliberately not
	// running, and showing it as simply "stopped" reads as a fault.
	Pools     int    `json:"pools"`
	Running   bool   `json:"running"`
	CLIPath   string `json:"cli_path,omitempty"` // binary WP-CLI and cron run
	IsDefault bool   `json:"is_default,omitempty"`

	// IonCube reports whether the loader is present for this version.
	//
	// Worth surfacing on its own rather than leaving in a phpinfo() page:
	// encoded commercial software simply does not run without it, and
	// "my application shows a blank page" is otherwise a long conversation.
	IonCube bool `json:"ioncube"`
}

// Provider names.
const (
	ProviderRemi   = "remi"
	ProviderAltPHP = "altphp"
)

// Provider installs PHP versions and says where their files are.
//
// Path methods take a version that the caller has already validated with
// ValidVersion; they build a path rather than checking one, so they cannot
// fail and have no error return.
type Provider interface {
	// Name is one of the provider constants.
	Name() string
	// Supported lists the versions this provider can install, newest last.
	Supported() []string
	// Available reports the state of every supported version.
	Available(ctx context.Context) ([]Version, error)
	// Install adds a version along with the extension set the panel expects.
	Install(ctx context.Context, version string) error
	// Uninstall removes a version.
	Uninstall(ctx context.Context, version string) error

	// CLIBinary is the php binary for WP-CLI, cron and the terminal.
	CLIBinary(version string) string
	// IniDropIn is the file the panel writes its php.ini overrides to.
	IniDropIn(version string) string
	// IonCubeLoader is the loader's shared object for this version. It may
	// not exist; Available reports whether it does.
	IonCubeLoader(version string) string
}

// FPMProvider is a Provider whose interpreter runs as pools of its own
// processes that the webserver reaches over a unix socket.
//
// Separate from Provider because a provider that is not pool-shaped is still
// plausible -- CloudLinux alt-php with LiteSpeed's LSAPI is one -- and code
// that needs pools should have to ask rather than assume.
type FPMProvider interface {
	Provider
	// PoolDir is where this version's pool definitions live.
	PoolDir(version string) string
	// PoolFile is the panel's definition for one named pool.
	PoolFile(version, pool string) string
	// SocketPath is the socket that pool listens on.
	SocketPath(version, pool string) string
	// ServiceUnit runs this version's pools.
	ServiceUnit(version string) string
	// FPMBinary is the pool manager itself; its presence means the version
	// is installed.
	FPMBinary(version string) string
}

// ErrUnsupportedVersion is returned for a version the provider cannot install.
type ErrUnsupportedVersion struct {
	Version   string
	Provider  string
	Supported []string
}

func (e *ErrUnsupportedVersion) Error() string {
	return fmt.Sprintf("php %s is not available from provider %q (supported: %v)",
		e.Version, e.Provider, e.Supported)
}

// checkVersion validates shape and support in one place, so every provider
// method rejects the same inputs.
func checkVersion(p Provider, v string) error {
	if !ValidVersion(v) {
		return fmt.Errorf("php version %q is malformed", v)
	}
	if !slices.Contains(p.Supported(), v) {
		return &ErrUnsupportedVersion{Version: v, Provider: p.Name(), Supported: p.Supported()}
	}
	return nil
}

// Providers lists every provider, in the order a person should be offered
// them.
var Providers = []string{ProviderRemi, ProviderAltPHP}

// StateFile records which provider the host runs sites on.
//
// A file beside the webserver's, for the same reason: the agent is the side
// that has to know and it has no database access. Written by the migration
// action, which is root, and read on every render.
const StateFile = "/var/lib/opanel/php/provider"

// DefaultProvider is what a host runs when nothing has said otherwise.
//
// Remi, not alt-php, because a panel is installed before CloudLinux is --
// that is the order the installer documents and the only order that works,
// since CloudLinux is a conversion of the running system. A host that has
// since been converted and migrated says so in the state file.
const DefaultProvider = ProviderRemi

// Active returns the provider the host is set to run sites on.
//
// A missing or unreadable state file gives the default rather than an error.
// Guessing wrong here is not silent: every path this provider produces is
// checked against the filesystem by Available, and a site pointed at a
// version that is not installed fails the render rather than serving
// something unexpected.
func Active() string {
	data, err := os.ReadFile(StateFile)
	if err == nil {
		name := strings.TrimSpace(string(data))
		if slices.Contains(Providers, name) {
			return name
		}
	}
	return DefaultProvider
}

// SetActive records the provider the host now runs sites on.
func SetActive(name string) error {
	if !slices.Contains(Providers, name) {
		return fmt.Errorf("php provider %q is not one of %v", name, Providers)
	}
	if err := os.MkdirAll(filepath.Dir(StateFile), 0o750); err != nil {
		return err
	}
	return os.WriteFile(StateFile, []byte(name+"\n"), 0o640)
}

// New builds the named provider.
func New(name string) (Provider, error) {
	switch name {
	case ProviderRemi:
		return NewRemi(), nil
	case ProviderAltPHP:
		return NewAltPHP(), nil
	default:
		return nil, fmt.Errorf("php provider %q is not one of %v", name, Providers)
	}
}

// ActiveProvider builds the provider the host is set to run.
func ActiveProvider() Provider {
	p, err := New(Active())
	if err != nil {
		return NewRemi()
	}
	return p
}

// ForBackend returns the PHP provider that goes with a webserver.
//
// The same one either way, and that is the arrangement rather than a
// coincidence: Apache reaches PHP through a pool socket and LiteSpeed spawns
// lsphp from the same tree, so both run the identical build. A failover
// between the two servers therefore cannot change which extensions a site
// has or which php.ini it reads -- which is the property an automatic
// failover lives or dies by.
func ForBackend(string) Provider { return ActiveProvider() }
