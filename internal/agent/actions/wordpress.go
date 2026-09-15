package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/dbms"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// WPCLIPath is where the installer puts WP-CLI.
//
// Under /usr/local/lib rather than on the PATH: customers reach this through
// the panel, and a wp on every shell's PATH is an invitation to run it as
// root, which WP-CLI itself refuses for good reason.
const WPCLIPath = "/usr/local/lib/opanel/wp-cli.phar"

// WPBudget bounds an install. Downloading core and running the installer
// against a cold database takes longer than the default action budget on a
// small VPS, and a WordPress install that dies halfway leaves a half-built
// site somebody has to clean up by hand.
const WPBudget = 15 * time.Minute

// wpTitlePattern keeps a site title to printable text on one line. The title
// reaches the shell only as an argv element, never as a shell string, so this
// is about sanity rather than injection.
var wpTitlePattern = regexp.MustCompile(`^[^\x00-\x1f]{1,120}$`)

// wpLocalePattern matches a WordPress locale such as en_US or vi.
var wpLocalePattern = regexp.MustCompile(`^[a-z]{2,3}(_[A-Z]{2})?$`)

// wpEmailPattern is deliberately loose. Rejecting a valid address is a worse
// failure than accepting an odd one, and WordPress checks it again anyway.
var wpEmailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s.]+\.[^@\s]+$`)

// WPInstallRequest installs WordPress into an existing site.
type WPInstallRequest struct {
	Owner        string `json:"owner"`
	DocumentRoot string `json:"document_root"`
	PHPVersion   string `json:"php_version"`
	SiteURL      string `json:"site_url"`

	DBName     string `json:"db_name"`
	DBUser     string `json:"db_user"`
	DBPassword string `json:"db_password"`

	Title         string `json:"title"`
	AdminUser     string `json:"admin_user"`
	AdminPassword string `json:"admin_password"`
	AdminEmail    string `json:"admin_email"`
	Locale        string `json:"locale"`
}

// Validate checks everything before a single file is written.
func (r *WPInstallRequest) Validate() error {
	if !linuxuser.ValidOwner(r.Owner) {
		return fmt.Errorf("%q is not an acceptable account name", r.Owner)
	}
	if !phpmgr.ValidVersion(r.PHPVersion) {
		return fmt.Errorf("%q is not a PHP version this panel supports", r.PHPVersion)
	}
	// The document root is built by the panel from the owner and the domain,
	// so it must sit under that owner's home. A request that names somewhere
	// else is a bug or an attack, not a configuration choice.
	if !strings.HasPrefix(r.DocumentRoot, linuxuser.Home(r.Owner)+"/") || strings.Contains(r.DocumentRoot, "..") {
		return fmt.Errorf("document root %q is not inside %s", r.DocumentRoot, linuxuser.Home(r.Owner))
	}
	if !strings.HasPrefix(r.SiteURL, "http://") && !strings.HasPrefix(r.SiteURL, "https://") {
		return fmt.Errorf("site url %q must start with http:// or https://", r.SiteURL)
	}
	if !dbms.ValidDatabaseName(r.DBName) {
		return fmt.Errorf("%q is not an acceptable database name", r.DBName)
	}
	if !dbms.ValidUserName(r.DBUser) {
		return fmt.Errorf("%q is not an acceptable database account name", r.DBUser)
	}
	if r.DBPassword == "" || r.AdminPassword == "" {
		return errors.New("both the database and administrator passwords are required")
	}
	if !wpTitlePattern.MatchString(r.Title) {
		return errors.New("the site title must be one line of printable text")
	}
	// WordPress usernames are permissive; this is the conservative subset,
	// which is what the panel generates anyway.
	if !regexp.MustCompile(`^[a-zA-Z0-9_.@-]{3,60}$`).MatchString(r.AdminUser) {
		return fmt.Errorf("%q is not an acceptable WordPress administrator name", r.AdminUser)
	}
	if !wpEmailPattern.MatchString(r.AdminEmail) {
		return fmt.Errorf("%q does not look like an email address", r.AdminEmail)
	}
	if r.Locale != "" && !wpLocalePattern.MatchString(r.Locale) {
		return fmt.Errorf("%q is not a WordPress locale", r.Locale)
	}
	return nil
}

// WPStatus reports what is in a document root.
type WPStatus struct {
	Installed bool   `json:"installed"`
	Empty     bool   `json:"empty"`
	Version   string `json:"version,omitempty"`
	SiteURL   string `json:"site_url,omitempty"`
	CLIReady  bool   `json:"cli_ready"`
}

// WPInstallResult reports a finished install.
type WPInstallResult struct {
	Version string `json:"version"`
	SiteURL string `json:"site_url"`
	Admin   string `json:"admin_user"`
}

// WPPathRequest asks about a document root.
type WPPathRequest struct {
	Owner        string `json:"owner"`
	DocumentRoot string `json:"document_root"`
	// PHPVersion is the interpreter WP-CLI should run under: the site's own,
	// so a plugin that needs an older PHP behaves in the manager the way it
	// behaves when the site is served. Optional -- an empty value falls back
	// to the newest installed version.
	PHPVersion string `json:"php_version,omitempty"`
}

// Validate checks the account and that the root is inside its home.
func (r *WPPathRequest) Validate() error {
	if !linuxuser.ValidOwner(r.Owner) {
		return fmt.Errorf("%q is not an acceptable account name", r.Owner)
	}
	if !strings.HasPrefix(r.DocumentRoot, linuxuser.Home(r.Owner)+"/") || strings.Contains(r.DocumentRoot, "..") {
		return fmt.Errorf("document root %q is not inside %s", r.DocumentRoot, linuxuser.Home(r.Owner))
	}
	return nil
}

func registerWordPress(r *agent.Registry, deps Deps) {
	agent.Register(r, "wp.status", 1, func(_ context.Context, in WPPathRequest) (WPStatus, error) {
		return wpStatus(in.DocumentRoot), nil
	})

	agent.RegisterSlow(r, "wp.install", 1, WPBudget,
		func(ctx context.Context, in WPInstallRequest) (WPInstallResult, error) {
			return installWordPress(ctx, deps, in)
		})
}

// wpStatus looks at the filesystem rather than asking WP-CLI: it has to
// answer for a directory that has no WordPress in it, which is the case the
// UI most needs to know about.
func wpStatus(docRoot string) WPStatus {
	st := WPStatus{CLIReady: fileExists(WPCLIPath)}

	entries, err := os.ReadDir(docRoot)
	if err != nil {
		return st
	}
	// The panel's own placeholder page does not count as content: a site
	// created a minute ago should still be offered a one-click install.
	meaningful := 0
	for _, e := range entries {
		if e.Name() == "index.html" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		meaningful++
	}
	st.Empty = meaningful == 0

	if fileExists(filepath.Join(docRoot, "wp-config.php")) ||
		fileExists(filepath.Join(docRoot, "wp-includes", "version.php")) {
		st.Installed = true
		st.Version = wpVersionFrom(filepath.Join(docRoot, "wp-includes", "version.php"))
	}
	return st
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// wpVersionFrom reads $wp_version out of version.php without running PHP.
var wpVersionRe = regexp.MustCompile(`\$wp_version\s*=\s*'([^']+)'`)

func wpVersionFrom(p string) string {
	body, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	if m := wpVersionRe.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	return ""
}

func installWordPress(ctx context.Context, deps Deps, in WPInstallRequest) (WPInstallResult, error) {
	acct, err := linuxuser.Lookup(in.Owner)
	if err != nil {
		return WPInstallResult{}, err
	}
	if acct == nil {
		return WPInstallResult{}, fmt.Errorf("account %q does not exist", in.Owner)
	}
	if !fileExists(WPCLIPath) {
		return WPInstallResult{}, fmt.Errorf(
			"WP-CLI is not installed at %s; run 'opanelctl install' to add it", WPCLIPath)
	}
	if st := wpStatus(in.DocumentRoot); st.Installed {
		return WPInstallResult{}, errors.New("this site already has WordPress in it")
	}

	php := deps.PHP().CLIBinary(in.PHPVersion)
	if !fileExists(php) {
		return WPInstallResult{}, fmt.Errorf("PHP %s is not installed on this server", in.PHPVersion)
	}

	// Everything runs as the site's own account, for two reasons. WP-CLI
	// refuses to run as root without --allow-root, and it is right to: a
	// plugin hook during install would run as root. And files created this
	// way are already owned correctly, with no chown pass to get wrong.
	wp := func(args ...string) error {
		argv := append([]string{
			"runuser", "-u", in.Owner, "--",
			// The site's own memory_limit is set for serving pages, and
			// extracting the WordPress archive needs more than a typical
			// 128M: the download step dies with "allowed memory size
			// exhausted" halfway through unpacking. Raised only for this
			// process, not for the site.
			php, "-d", "memory_limit=512M",
			WPCLIPath, "--path=" + in.DocumentRoot, "--no-color",
		}, args...)
		res, err := run.Cmd(ctx, argv, run.Timeout(WPBudget))
		if err != nil {
			return fmt.Errorf("wp %s: %s", strings.Join(args, " "),
				firstLines(res.Stderr+res.Stdout, 4))
		}
		return nil
	}

	if err := os.MkdirAll(in.DocumentRoot, 0o755); err != nil {
		return WPInstallResult{}, err
	}
	if err := os.Chown(in.DocumentRoot, int(acct.UID), int(acct.GID)); err != nil {
		return WPInstallResult{}, err
	}
	// The placeholder would otherwise be served ahead of index.php and the
	// customer would see "your site is ready" for ever.
	_ = os.Remove(filepath.Join(in.DocumentRoot, "index.html"))

	locale := in.Locale
	if locale == "" {
		locale = "en_US"
	}

	download := []string{"core", "download", "--locale=" + locale}
	if err := wp(download...); err != nil {
		return WPInstallResult{}, err
	}

	// --dbpass as an argv element, never interpolated into a command line:
	// there is no shell here, so a password with a quote in it is just a
	// password.
	if err := wp("config", "create",
		"--dbname="+in.DBName, "--dbuser="+in.DBUser, "--dbpass="+in.DBPassword,
		"--dbhost=localhost", "--skip-check"); err != nil {
		return WPInstallResult{}, err
	}

	if err := wp("core", "install",
		"--url="+in.SiteURL, "--title="+in.Title,
		"--admin_user="+in.AdminUser, "--admin_password="+in.AdminPassword,
		"--admin_email="+in.AdminEmail, "--skip-email"); err != nil {
		return WPInstallResult{}, err
	}

	// A fresh install ships with themes and plugins nobody asked for, and
	// every one of them is attack surface that still has to be patched.
	_ = wp("plugin", "delete", "hello", "akismet")

	if err := hardenWordPress(in.DocumentRoot, acct); err != nil {
		return WPInstallResult{}, err
	}

	return WPInstallResult{
		Version: wpVersionFrom(filepath.Join(in.DocumentRoot, "wp-includes", "version.php")),
		SiteURL: in.SiteURL,
		Admin:   in.AdminUser,
	}, nil
}

// hardenWordPress tightens what the install left behind.
func hardenWordPress(docRoot string, acct *linuxuser.Account) error {
	// wp-config.php holds the database password and is world-readable as
	// WP-CLI writes it. Other accounts share this host.
	cfg := filepath.Join(docRoot, "wp-config.php")
	if fileExists(cfg) {
		if err := os.Chmod(cfg, 0o600); err != nil {
			return fmt.Errorf("chmod wp-config.php: %w", err)
		}
		if err := os.Chown(cfg, int(acct.UID), int(acct.GID)); err != nil {
			return err
		}
	}
	// uploads has to exist and be writable before the first upload, or
	// WordPress reports a confusing permissions error.
	uploads := filepath.Join(docRoot, "wp-content", "uploads")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		return err
	}
	return os.Chown(uploads, int(acct.UID), int(acct.GID))
}

// firstLines trims command output down to something worth putting in an
// error message; WP-CLI is happy to print forty lines of PHP notices.
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "; ")
}
