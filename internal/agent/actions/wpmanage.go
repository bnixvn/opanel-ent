package actions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// WPManageBudget bounds an update. Core, plugins and themes are all fetched
// over the network from wordpress.org, which is slow often enough that a
// short budget turns a working update into a mystery.
const WPManageBudget = 20 * time.Minute

// WPSSOTTL is how long a one-click login link lives. Only long enough to
// follow a redirect: the token travels in a URL, which ends up in browser
// history and in the site's own access log.
const WPSSOTTL = 2 * time.Minute

// WPComponent is a plugin or a theme.
type WPComponent struct {
	Name       string `json:"name"`
	Title      string `json:"title,omitempty"`
	Status     string `json:"status"`
	Version    string `json:"version"`
	Update     string `json:"update,omitempty"` // the version available, if any
	AutoUpdate bool   `json:"auto_update"`
}

// WPInfo is everything the manager shows about one installation.
type WPInfo struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	// CoreUpdate is the newer core version available, empty when current.
	CoreUpdate string        `json:"core_update,omitempty"`
	SiteURL    string        `json:"site_url,omitempty"`
	HomeURL    string        `json:"home_url,omitempty"`
	AdminUser  string        `json:"admin_user,omitempty"`
	DBName     string        `json:"db_name,omitempty"`
	Plugins    []WPComponent `json:"plugins"`
	Themes     []WPComponent `json:"themes"`
	// Error carries what went wrong when WordPress is present but WP-CLI
	// could not talk to it -- a broken database password, most often. The
	// manager shows it rather than an empty page.
	Error string `json:"error,omitempty"`
}

// WPActionRequest runs one management command against an installation.
type WPActionRequest struct {
	Owner        string `json:"owner"`
	DocumentRoot string `json:"document_root"`
	PHPVersion   string `json:"php_version"`
	// Kind is "core", "plugin" or "theme".
	Kind string `json:"kind"`
	// Op is "update", "activate", "deactivate" or "delete".
	Op string `json:"op"`
	// Name is the plugin or theme slug. Empty with kind "plugin" or "theme"
	// and op "update" means everything with an update available.
	Name string `json:"name,omitempty"`
}

// wpSlugOK accepts the slug shapes WordPress itself uses. It is the guard
// that keeps a name from becoming a second argument.
func wpSlugOK(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	// A leading dot would let "..", and a slug is never a path.
	return !strings.HasPrefix(s, ".")
}

// Validate checks the target and the operation.
func (r *WPActionRequest) Validate() error {
	if err := (&WPPathRequest{Owner: r.Owner, DocumentRoot: r.DocumentRoot}).Validate(); err != nil {
		return err
	}
	switch r.Kind {
	case "core", "plugin", "theme":
	default:
		return fmt.Errorf("%q is not something this panel manages", r.Kind)
	}
	switch r.Op {
	case "update", "activate", "deactivate", "delete":
	default:
		return fmt.Errorf("%q is not an operation", r.Op)
	}
	if r.Kind == "core" && r.Op != "update" {
		return fmt.Errorf("core can only be updated")
	}
	if r.Name != "" && !wpSlugOK(r.Name) {
		return fmt.Errorf("%q is not a plugin or theme name", r.Name)
	}
	if r.Name == "" && r.Op != "update" {
		return fmt.Errorf("which plugin or theme?")
	}
	return nil
}

// WPActionResult reports what a command did.
type WPActionResult struct {
	Output string `json:"output"`
	Info   WPInfo `json:"info"`
}

// WPSSOResult is a one-click login link.
type WPSSOResult struct {
	URL       string    `json:"url"`
	User      string    `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
}

func registerWPManage(r *agent.Registry, deps Deps) {
	agent.RegisterSlow(r, "wp.info", 1, 3*time.Minute,
		func(ctx context.Context, in WPPathRequest) (WPInfo, error) {
			return wpInfo(ctx, deps, in, "")
		})

	agent.RegisterSlow(r, "wp.manage", 1, WPManageBudget,
		func(ctx context.Context, in WPActionRequest) (WPActionResult, error) {
			return wpManage(ctx, deps, in)
		})

	agent.RegisterSlow(r, "wp.sso", 1, 3*time.Minute,
		func(ctx context.Context, in WPPathRequest) (WPSSOResult, error) {
			return wpSSO(ctx, deps, in)
		})
}

// wpRunner returns a function that runs WP-CLI as the site's own account.
//
// As the account, never as root: WP-CLI refuses --allow-root for good reason,
// because a plugin hook fires during most of these commands and would run
// with whatever privilege it was given.
func wpRunner(ctx context.Context, deps Deps, owner, docRoot, phpVersion string) (func(...string) (string, error), error) {
	if !fileExists(WPCLIPath) {
		return nil, fmt.Errorf("WP-CLI is not installed at %s", WPCLIPath)
	}
	acct, err := linuxuser.Lookup(owner)
	if err != nil || acct == nil {
		return nil, fmt.Errorf("account %q does not exist", owner)
	}
	php := deps.PHP().CLIBinary(phpVersion)
	if phpVersion == "" || !fileExists(php) {
		// Falling back rather than failing: a site whose PHP version was
		// removed still has WordPress in it, and the manager should be able
		// to say so rather than showing nothing. Newest first, because a
		// plugin is likelier to work on a newer interpreter than an older.
		php = ""
		supported := deps.PHP().Supported()
		for i := len(supported) - 1; i >= 0; i-- {
			if candidate := deps.PHP().CLIBinary(supported[i]); fileExists(candidate) {
				php = candidate
				break
			}
		}
		if php == "" {
			return nil, fmt.Errorf("no PHP interpreter is installed to run WP-CLI")
		}
	}
	return func(args ...string) (string, error) {
		argv := append([]string{
			"runuser", "-u", owner, "--",
			php, "-d", "memory_limit=512M",
			WPCLIPath, "--path=" + docRoot, "--no-color",
		}, args...)
		res, err := run.Cmd(ctx, argv, run.Timeout(WPManageBudget))
		if err != nil {
			return res.Stdout, fmt.Errorf("wp %s: %s", strings.Join(args, " "),
				firstLines(res.Stderr+res.Stdout, 4))
		}
		return res.Stdout, nil
	}, nil
}

// wpInfo collects what the manager displays.
func wpInfo(ctx context.Context, deps Deps, in WPPathRequest, extraOutput string) (WPInfo, error) {
	st := wpStatus(in.DocumentRoot)
	info := WPInfo{Installed: st.Installed, Version: st.Version}
	if !st.Installed {
		return info, nil
	}

	wp, err := wpRunner(ctx, deps, in.Owner, in.DocumentRoot, in.PHPVersion)
	if err != nil {
		info.Error = err.Error()
		return info, nil
	}

	// Everything below is best effort. A site whose database password is
	// wrong still has a version on disk worth showing, and an install that
	// cannot reach wordpress.org should not look broken.
	if out, err := wp("option", "get", "siteurl"); err == nil {
		info.SiteURL = strings.TrimSpace(out)
	} else {
		info.Error = err.Error()
		return info, nil
	}
	if out, err := wp("option", "get", "home"); err == nil {
		info.HomeURL = strings.TrimSpace(out)
	}
	if out, err := wp("config", "get", "DB_NAME"); err == nil {
		info.DBName = strings.TrimSpace(out)
	}
	if out, err := wp("user", "list", "--role=administrator", "--field=user_login", "--number=1"); err == nil {
		info.AdminUser = strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	}
	if out, err := wp("core", "check-update", "--format=json", "--fields=version"); err == nil {
		var rows []struct {
			Version string `json:"version"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(out)), &rows) == nil && len(rows) > 0 {
			info.CoreUpdate = rows[0].Version
		}
	}
	info.Plugins = wpComponents(wp, "plugin")
	info.Themes = wpComponents(wp, "theme")
	if extraOutput != "" {
		info.Error = ""
	}
	return info, nil
}

// wpComponents lists plugins or themes with their update state.
func wpComponents(wp func(...string) (string, error), kind string) []WPComponent {
	out, err := wp(kind, "list", "--format=json",
		"--fields=name,title,status,version,update_version,auto_update")
	if err != nil {
		return nil
	}
	var rows []struct {
		Name          string `json:"name"`
		Title         string `json:"title"`
		Status        string `json:"status"`
		Version       string `json:"version"`
		UpdateVersion string `json:"update_version"`
		AutoUpdate    string `json:"auto_update"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &rows) != nil {
		return nil
	}
	list := make([]WPComponent, 0, len(rows))
	for _, r := range rows {
		list = append(list, WPComponent{
			Name: r.Name, Title: r.Title, Status: r.Status, Version: r.Version,
			Update: r.UpdateVersion, AutoUpdate: r.AutoUpdate == "on",
		})
	}
	return list
}

// wpManage runs one update or state change.
func wpManage(ctx context.Context, deps Deps, in WPActionRequest) (WPActionResult, error) {
	if st := wpStatus(in.DocumentRoot); !st.Installed {
		return WPActionResult{}, fmt.Errorf("there is no WordPress in %s", in.DocumentRoot)
	}
	wp, err := wpRunner(ctx, deps, in.Owner, in.DocumentRoot, in.PHPVersion)
	if err != nil {
		return WPActionResult{}, err
	}

	var args []string
	switch {
	case in.Kind == "core":
		args = []string{"core", "update"}
	case in.Op == "update" && in.Name == "":
		args = []string{in.Kind, "update", "--all"}
	case in.Op == "update":
		args = []string{in.Kind, "update", in.Name}
	case in.Op == "delete":
		args = []string{in.Kind, "delete", in.Name}
	default:
		args = []string{in.Kind, in.Op, in.Name}
	}

	out, runErr := wp(args...)
	if in.Kind == "core" && runErr == nil {
		// Core files and the database schema are two separate steps, and a
		// site left between them shows the update screen to every visitor.
		if dbOut, err := wp("core", "update-db"); err == nil {
			out += "\n" + dbOut
		}
	}
	if runErr != nil {
		return WPActionResult{Output: out}, runErr
	}

	info, _ := wpInfo(ctx, deps, WPPathRequest{
		Owner: in.Owner, DocumentRoot: in.DocumentRoot, PHPVersion: in.PHPVersion,
	}, out)
	return WPActionResult{Output: strings.TrimSpace(out), Info: info}, nil
}

// wpSSO mints a one-click link into wp-admin.
//
// The panel cannot know the customer's WordPress password, and resetting one
// to get in would lock the customer out of their own site. Instead a token is
// stored as a WordPress transient -- inside the site's own database, where
// only that site can read it -- and a small must-use plugin exchanges it for
// a session exactly once.
func wpSSO(ctx context.Context, deps Deps, in WPPathRequest) (WPSSOResult, error) {
	if st := wpStatus(in.DocumentRoot); !st.Installed {
		return WPSSOResult{}, fmt.Errorf("there is no WordPress in %s", in.DocumentRoot)
	}
	acct, err := linuxuser.Lookup(in.Owner)
	if err != nil || acct == nil {
		return WPSSOResult{}, fmt.Errorf("account %q does not exist", in.Owner)
	}
	wp, err := wpRunner(ctx, deps, in.Owner, in.DocumentRoot, in.PHPVersion)
	if err != nil {
		return WPSSOResult{}, err
	}

	out, err := wp("user", "list", "--role=administrator", "--field=ID", "--number=1")
	if err != nil {
		return WPSSOResult{}, fmt.Errorf("find an administrator: %w", err)
	}
	userID := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	if userID == "" {
		return WPSSOResult{}, fmt.Errorf("this WordPress has no administrator to sign in as")
	}
	login, _ := wp("user", "get", userID, "--field=user_login")

	if err := installWPSSOPlugin(in.DocumentRoot, acct); err != nil {
		return WPSSOResult{}, err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return WPSSOResult{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	// The hash is stored, not the token: a copy of the site's database is
	// not a way in.
	if _, err := wp("transient", "set",
		"opanel_sso_"+hex.EncodeToString(sum[:]), userID,
		fmt.Sprintf("%d", int(WPSSOTTL.Seconds()))); err != nil {
		return WPSSOResult{}, fmt.Errorf("store the sign-in token: %w", err)
	}

	site, _ := wp("option", "get", "siteurl")
	base := strings.TrimRight(strings.TrimSpace(site), "/")
	return WPSSOResult{
		URL:       base + "/?opanel_sso=" + token,
		User:      strings.TrimSpace(login),
		ExpiresAt: time.Now().Add(WPSSOTTL),
	}, nil
}

// installWPSSOPlugin writes the must-use plugin that redeems a token.
func installWPSSOPlugin(docRoot string, acct *linuxuser.Account) error {
	dir := filepath.Join(docRoot, "wp-content", "mu-plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chown(dir, int(acct.UID), int(acct.GID)); err != nil {
		return err
	}
	path := filepath.Join(dir, "opanel-sso.php")
	if err := os.WriteFile(path, []byte(wpSSOPlugin), 0o644); err != nil {
		return err
	}
	return os.Chown(path, int(acct.UID), int(acct.GID))
}

// wpSSOPlugin redeems a one-time token for a session.
//
// It is a must-use plugin so a customer cannot deactivate it by accident and
// then wonder why the panel button stopped working. It does nothing at all
// unless the exact query parameter is present, the token matches a transient
// the panel wrote minutes earlier, and that transient has not been used --
// deleting it is the first thing that happens, so a replayed link is dead
// before the session is created.
const wpSSOPlugin = `<?php
/**
 * Plugin Name: OPanel one-click sign-in
 * Description: Exchanges a one-time token from the hosting panel for a session. Managed by OPanel.
 */

if (!defined('ABSPATH')) {
    exit;
}

add_action('init', function () {
    if (empty($_GET['opanel_sso']) || !is_string($_GET['opanel_sso'])) {
        return;
    }
    $token = $_GET['opanel_sso'];
    if (!preg_match('/^[A-Za-z0-9_-]{20,128}$/', $token)) {
        return;
    }

    $key = 'opanel_sso_' . hash('sha256', $token);
    $user_id = get_transient($key);
    if ($user_id === false) {
        wp_die('This sign-in link has expired or has already been used. Open it again from the panel.',
            'Link expired', ['response' => 403]);
    }

    // Consumed before the session exists, so two requests racing with the
    // same link cannot both win.
    delete_transient($key);

    $user = get_user_by('id', (int) $user_id);
    if (!$user) {
        wp_die('That account no longer exists.', 'Sign-in failed', ['response' => 403]);
    }

    wp_set_current_user($user->ID);
    wp_set_auth_cookie($user->ID, false, is_ssl());
    do_action('wp_login', $user->user_login, $user);

    wp_safe_redirect(admin_url());
    exit;
});
`
