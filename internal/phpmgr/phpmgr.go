// Package phpmgr abstracts where PHP comes from.
//
// Two providers are planned. Stage A uses lsphp packages from the LiteSpeed
// repository; Stage B switches to CloudLinux alt-php, which additionally
// offers PHP older than 8.1 and HardenedPHP patches for end-of-life versions.
//
// The abstraction earns its place at exactly one point: a site stores the
// string "8.4", never a binary path. Changing provider is then a re-render of
// every vhost rather than a data migration.
package phpmgr

import (
	"context"
	"fmt"
	"regexp"
	"slices"
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
	LSAPIPath string `json:"lsapi_path,omitempty"` // binary the webserver runs
	CLIPath   string `json:"cli_path,omitempty"`   // binary WP-CLI and cron run
	IsDefault bool   `json:"is_default,omitempty"`
}

// Provider installs PHP versions and says where their files are.
//
// Path methods take a version that the caller has already validated with
// ValidVersion; they build a path rather than checking one, so they cannot
// fail and have no error return.
type Provider interface {
	// Name is "lsphp" or "altphp".
	Name() string
	// Supported lists the versions this provider can install, newest last.
	Supported() []string
	// Available reports the state of every supported version.
	Available(ctx context.Context) ([]Version, error)
	// Install adds a version along with the extension set the panel expects.
	Install(ctx context.Context, version string) error
	// Uninstall removes a version.
	Uninstall(ctx context.Context, version string) error

	// LSAPIBinary is the lsphp binary a vhost points at.
	LSAPIBinary(version string) string
	// CLIBinary is the php binary for WP-CLI, cron and the terminal.
	CLIBinary(version string) string
	// IniDropIn is the file the panel writes its php.ini overrides to.
	IniDropIn(version string) string
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

// Detect picks a provider for the host. CloudLinux hosts get alt-php once
// that provider exists; everything else gets lsphp.
func Detect(cloudLinux bool) Provider {
	// Stage B will return the alt-php provider here. Until it exists, lsphp
	// works on CloudLinux too -- the LiteSpeed repository supports EL10
	// regardless of whether the CloudLinux subsystem is installed.
	_ = cloudLinux
	return NewLSPHP()
}
