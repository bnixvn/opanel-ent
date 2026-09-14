// Package webserver renders and applies webserver configuration.
//
// Two backends are planned: OpenLiteSpeed for Stage A and LiteSpeed
// Enterprise for Stage B. They share nothing but this interface -- OLS uses a
// plain-text config format and LSWS Enterprise uses XML -- so the seam is the
// whole configuration, not individual directives.
//
// The design constraint that shapes everything here: the panel must be able to
// switch backends at runtime, including automatically when a LiteSpeed licence
// lapses. That is only safe if a complete, correct configuration can be
// regenerated from the database at any moment. So Render takes every site at
// once and is a pure function, and Apply replaces the whole managed tree
// rather than patching it.
package webserver

import (
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Backend names.
const (
	BackendOLS  = "ols"
	BackendLSWS = "lsws"
)

// App types a site can be.
const (
	AppStatic    = "static"
	AppPHP       = "php"
	AppWordPress = "wordpress"
)

// AppTypes lists every valid app type.
var AppTypes = []string{AppStatic, AppPHP, AppWordPress}

// Rewrite modes.
const (
	RewriteNone            = "none"
	RewriteFrontController = "front_controller"
	RewriteLaravel         = "laravel"
	RewriteCodeIgniter     = "codeigniter"
)

// RewriteModes lists every valid rewrite mode.
var RewriteModes = []string{RewriteNone, RewriteFrontController, RewriteLaravel, RewriteCodeIgniter}

// domainPattern is deliberately strict. A domain becomes a directory name, a
// config block name and a log filename, so anything outside this shape is
// rejected before it can reach a path or a directive.
var domainPattern = regexp.MustCompile(
	`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// ValidDomain reports whether d is an acceptable hostname.
func ValidDomain(d string) bool {
	return len(d) <= 253 && domainPattern.MatchString(d)
}

// unixNamePattern matches a Linux account name the panel may run PHP as.
var unixNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// ValidUnixName reports whether n is an acceptable Linux user or group name.
func ValidUnixName(n string) bool { return unixNamePattern.MatchString(n) }

// Site is everything a backend needs to render one vhost. It is built from a
// database row plus paths resolved by the active PHP provider, so a renderer
// never has to look anything up.
type Site struct {
	Domain  string
	Aliases []string

	// VhostRoot is the per-site directory, normally /home/<user>/<domain>.
	// Logs live under it so the owner can read them through the file manager
	// and so they count against the owner's disk quota.
	VhostRoot    string
	DocumentRoot string
	AppType      string
	RewriteMode  string

	// OwnerUser and OwnerGroup are the Linux account PHP runs as. Per-site
	// suEXEC is what keeps one customer's code out of another's files.
	OwnerUser  string
	OwnerGroup string

	// PHPVersion is informational; LSAPIBinary is what the renderer emits.
	// Both are empty for a static site.
	PHPVersion  string
	LSAPIBinary string

	SSLEnabled bool
	CertFile   string
	KeyFile    string
	// ForceHTTPS redirects plain HTTP to TLS. Per-site rather than global: a
	// site behind a CDN that terminates TLS elsewhere must not be forced into
	// a redirect loop.
	ForceHTTPS bool

	WAFEnabled bool
	Suspended  bool
}

// NeedsPHP reports whether the site runs an interpreter.
func (s Site) NeedsPHP() bool {
	return s.AppType == AppPHP || s.AppType == AppWordPress
}

// Validate checks a site before rendering. Every renderer calls it, so a
// malformed site cannot reach a template in any backend.
func (s Site) Validate() error {
	if !ValidDomain(s.Domain) {
		return fmt.Errorf("domain %q is not a valid hostname", s.Domain)
	}
	for _, a := range s.Aliases {
		if !ValidDomain(a) {
			return fmt.Errorf("alias %q is not a valid hostname", a)
		}
		if a == s.Domain {
			return fmt.Errorf("alias %q duplicates the primary domain", a)
		}
	}
	if !slices.Contains(AppTypes, s.AppType) {
		return fmt.Errorf("app type %q is not one of %v", s.AppType, AppTypes)
	}
	if !slices.Contains(RewriteModes, s.RewriteMode) {
		return fmt.Errorf("rewrite mode %q is not one of %v", s.RewriteMode, RewriteModes)
	}
	if !ValidUnixName(s.OwnerUser) {
		return fmt.Errorf("owner user %q is not a valid account name", s.OwnerUser)
	}
	if !ValidUnixName(s.OwnerGroup) {
		return fmt.Errorf("owner group %q is not a valid group name", s.OwnerGroup)
	}
	if err := validAbsPath(s.VhostRoot, "vhost root"); err != nil {
		return err
	}
	if err := validAbsPath(s.DocumentRoot, "document root"); err != nil {
		return err
	}
	// Containment matters: the document root is what the world can reach, so
	// it must not escape the directory the panel owns for this site.
	if s.DocumentRoot != s.VhostRoot && !strings.HasPrefix(s.DocumentRoot, s.VhostRoot+"/") {
		return fmt.Errorf("document root %q is outside vhost root %q", s.DocumentRoot, s.VhostRoot)
	}
	if s.NeedsPHP() {
		if s.LSAPIBinary == "" {
			return fmt.Errorf("site %q needs PHP but no interpreter was resolved", s.Domain)
		}
		if err := validAbsPath(s.LSAPIBinary, "php binary"); err != nil {
			return err
		}
	} else if s.LSAPIBinary != "" {
		return fmt.Errorf("static site %q must not carry a php binary", s.Domain)
	}
	if s.SSLEnabled {
		if err := validAbsPath(s.CertFile, "certificate"); err != nil {
			return err
		}
		if err := validAbsPath(s.KeyFile, "private key"); err != nil {
			return err
		}
	}
	return nil
}

// validAbsPath rejects relative paths and traversal. Paths reach config files
// verbatim, so a "..", a newline or a brace would let a value break out of the
// directive it belongs to.
func validAbsPath(p, what string) error {
	switch {
	case p == "":
		return fmt.Errorf("%s must not be empty", what)
	case !path.IsAbs(p):
		return fmt.Errorf("%s %q must be an absolute path", what, p)
	case p != path.Clean(p):
		return fmt.Errorf("%s %q is not a clean path", what, p)
	case strings.ContainsAny(p, "\n\r\x00{}\""):
		return fmt.Errorf("%s %q contains characters not allowed in a config file", what, p)
	}
	return nil
}

// LogDir is where this site's access and error logs are written.
func (s Site) LogDir() string { return path.Join(s.VhostRoot, "logs") }

// Hostnames returns the primary domain followed by its aliases.
func (s Site) Hostnames() []string {
	return append([]string{s.Domain}, s.Aliases...)
}

// ServerConfig holds the server-level settings a backend needs.
type ServerConfig struct {
	HTTPPort  int
	HTTPSPort int
	// ServerUser and ServerGroup are what the webserver's own workers run as.
	// Per-site PHP runs as the site owner instead.
	ServerUser  string
	ServerGroup string
	AdminEmail  string
	// PanelManagedComment is written at the top of every generated file.
	PanelManagedComment string
	// WAFRulesFile is the ModSecurity configuration to load, empty when the
	// engine or its rules are not installed.
	//
	// Passed in rather than probed by the renderer: a renderer that looks at
	// the filesystem produces different output on different machines, which
	// makes it untestable and makes a config change depend on where it ran.
	WAFRulesFile string
}

// DefaultServerConfig returns the settings a fresh install uses.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		HTTPPort:    80,
		HTTPSPort:   443,
		ServerUser:  "nobody",
		ServerGroup: "nobody",
		AdminEmail:  "root@localhost",
		PanelManagedComment: "Generated by OPanel. Do not edit: this file is rewritten\n" +
			"# from the panel database on every change, and edits made here\n" +
			"# or in the WebAdmin console will be lost.",
	}
}

// SuspendedRoot holds the page served for a suspended site. One shared
// directory rather than one per site: the content is identical, and a
// suspended site must not be able to change what its own suspension notice
// says.
//
// Deliberately not under /var/lib/opanel. That directory is the panel's
// private state, mode 0750 and owned by the opanel account, so the webserver
// worker cannot traverse into it -- serving the notice from there produced a
// 403 instead of the notice.
const SuspendedRoot = "/usr/share/opanel/suspended"

// ACMEWebroot is the shared directory HTTP-01 challenges are served from.
//
// One directory for every domain on the host. The alternative -- a challenge
// path inside each site's own document root -- would put the panel's files
// inside a customer's tree and break the moment they deploy something that
// rewrites every request to a front controller.
const ACMEWebroot = "/usr/share/opanel/acme"

// ACMEVhostName is the catch-all vhost that answers challenges for hostnames
// no site claims yet -- which is every hostname at the moment its first
// certificate is issued.
const ACMEVhostName = "_acme"

// File is one rendered configuration file.
type File struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

// Rendered is a complete configuration: the main file plus one file per site.
type Rendered struct {
	Main   File
	Vhosts []File
	// VhostDirs are the directories that must exist and hold nothing else.
	// Apply prunes anything under the managed root that is not listed here,
	// which is how a deleted site's config disappears.
	VhostDirs []string
}

// Files returns every file in the render, main first.
func (r Rendered) Files() []File {
	return append([]File{r.Main}, r.Vhosts...)
}

// Backend renders and applies configuration for one webserver.
type Backend interface {
	// Name is BackendOLS or BackendLSWS.
	Name() string
	// Installed reports whether the webserver is present on the host.
	Installed() bool
	// ServiceUnit is the systemd unit that runs it.
	ServiceUnit() string

	// Render is pure: same inputs, same bytes, no I/O. This is what makes
	// golden-file tests the real safety net -- see TestConfig for why they
	// have to be.
	Render(cfg ServerConfig, sites []Site) (Rendered, error)

	// Apply writes a render, validates it, reloads, and rolls back to the
	// previous configuration if either step fails.
	Apply(ctx context.Context, r Rendered) error

	// TestConfig asks the webserver to parse its configuration.
	//
	// Callers must not treat this as a full check. OpenLiteSpeed accepts
	// unknown directives with only a warning, so a misspelled directive
	// passes here and then silently does nothing at runtime.
	TestConfig(ctx context.Context) error

	// Reload applies configuration without dropping connections.
	Reload(ctx context.Context) error
}
