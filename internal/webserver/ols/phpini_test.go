package ols

import (
	"strings"
	"testing"

	"github.com/bnixvn/opanel-ent/internal/webserver"
)

func vhostFor(t *testing.T, r webserver.Rendered, domain string) string {
	t.Helper()
	for _, v := range r.Vhosts {
		if strings.Contains(v.Path, domain) {
			return string(v.Content)
		}
	}
	t.Fatalf("no vhost was rendered for %s", domain)
	return ""
}

func TestRenderPHPSettings(t *testing.T) {
	b := newBackend(t)
	s := testSite("ini.example.com", "alice", webserver.AppPHP, "8.4")
	s.PHPSettings = map[string]string{
		"memory_limit":   "512M",
		"display_errors": "On",
		"date.timezone":  "Asia/Ho_Chi_Minh",
	}
	r, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := vhostFor(t, r, "ini.example.com")

	want := "phpIniOverride {\n" +
		"php_admin_value date.timezone Asia/Ho_Chi_Minh\n" +
		"php_admin_flag display_errors On\n" +
		"php_admin_value memory_limit 512M\n" +
		"}\n"
	if !strings.Contains(got, want) {
		t.Fatalf("the override block is not as expected. Rendered:\n%s", got)
	}
	// It has to sit inside the vhost, after the handler that runs PHP;
	// OpenLiteSpeed applies it to the site's own interpreter.
	if strings.Index(got, "scripthandler") > strings.Index(got, "phpIniOverride") {
		t.Fatal("the override block was emitted before the script handler")
	}
}

// A site with no overrides must render exactly as it did before the feature
// existed -- that is what the golden files assert, and it is why the block is
// trimmed rather than emitted empty.
func TestRenderWithoutPHPSettingsEmitsNothing(t *testing.T) {
	b := newBackend(t)
	s := testSite("plain.example.com", "alice", webserver.AppPHP, "8.4")
	r, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(vhostFor(t, r, "plain.example.com"), "phpIniOverride") {
		t.Fatal("an empty override block was emitted")
	}
}

// A suspended site serves a notice page with no interpreter at all, so
// directives for one would be attached to nothing.
func TestSuspendedSiteDropsPHPSettings(t *testing.T) {
	b := newBackend(t)
	s := testSite("off.example.com", "alice", webserver.AppPHP, "8.4")
	s.PHPSettings = map[string]string{"memory_limit": "512M"}
	s.Suspended = true
	r, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(vhostFor(t, r, "off.example.com"), "phpIniOverride") {
		t.Fatal("a suspended site was given php settings")
	}
}

// Validate is the last gate before a value is written into the webserver's
// configuration, so it has to refuse what should never have reached it.
func TestRenderRefusesUnvettedPHPSettings(t *testing.T) {
	cases := map[string]map[string]string{
		"a directive nobody vetted": {"auto_prepend_file": "/tmp/shell.php"},
		"a value with a newline":    {"memory_limit": "512M\nphp_admin_value disable_functions none"},
		"a value with a brace":      {"memory_limit": "512M}"},
	}
	for name, settings := range cases {
		t.Run(name, func(t *testing.T) {
			b := newBackend(t)
			s := testSite("bad.example.com", "alice", webserver.AppPHP, "8.4")
			s.PHPSettings = settings
			if _, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s}); err == nil {
				t.Fatal("an unvetted php setting reached the renderer")
			}
		})
	}
}

func TestStaticSiteCannotCarryPHPSettings(t *testing.T) {
	b := newBackend(t)
	s := testSite("static.example.com", "alice", webserver.AppStatic, "")
	s.PHPSettings = map[string]string{"memory_limit": "512M"}
	if _, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s}); err == nil {
		t.Fatal("a static site was allowed php settings")
	}
}
