package ols

import (
	"flag"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnixvn/opanel-ent/internal/webserver"
)

var update = flag.Bool("update", false, "rewrite golden files")

// These tests are the real safety net for the renderer, not TestConfig.
// OpenLiteSpeed accepts an unknown directive with only a warning, so a
// misspelled directive would pass a config test and then silently do nothing.
// Comparing exact bytes is the only thing that catches it.

func testSite(domain, owner, appType, phpVersion string) webserver.Site {
	root := "/home/" + owner + "/" + domain
	s := webserver.Site{
		Domain:       domain,
		VhostRoot:    root,
		DocumentRoot: root + "/public_html",
		AppType:      appType,
		RewriteMode:  webserver.RewriteNone,
		OwnerUser:    owner,
		OwnerGroup:   owner,
		PHPVersion:   phpVersion,
	}
	if s.NeedsPHP() {
		s.LSAPIBinary = "/usr/local/lsws/lsphp" + strings.ReplaceAll(phpVersion, ".", "") + "/bin/lsphp"
	}
	return s
}

func goldenCheck(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run: go test ./internal/webserver/ols -update): %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from golden.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// siteVhost returns the rendered config for one domain. The render also
// contains the ACME catch-all, so positional access is not safe.
func siteVhost(t *testing.T, r webserver.Rendered, domain string) []byte {
	t.Helper()
	want := ManagedDir + "/vhosts/" + domain + "/vhconf.conf"
	for _, f := range r.Vhosts {
		if f.Path == want {
			return f.Content
		}
	}
	t.Fatalf("no vhost rendered for %q", domain)
	return nil
}

func newBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New()
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	return b
}

func TestRenderGolden(t *testing.T) {
	b := newBackend(t)
	cfg := webserver.DefaultServerConfig()

	wp := testSite("example.com", "alice", webserver.AppWordPress, "8.4")
	wp.Aliases = []string{"www.example.com"}

	laravel := testSite("app.example.net", "bob", webserver.AppPHP, "8.1")
	laravel.RewriteMode = webserver.RewriteLaravel

	static := testSite("static.example.org", "carol", webserver.AppStatic, "")

	secure := testSite("secure.example.com", "dave", webserver.AppPHP, "8.3")
	secure.SSLEnabled = true
	secure.CertFile = "/var/lib/opanel/ssl/secure.example.com/fullchain.pem"
	secure.KeyFile = "/var/lib/opanel/ssl/secure.example.com/privkey.pem"

	suspended := testSite("suspended.example.com", "erin", webserver.AppPHP, "8.4")
	suspended.Suspended = true

	sites := []webserver.Site{wp, laravel, static, secure, suspended}

	r, err := b.Render(cfg, sites)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	goldenCheck(t, "httpd_config.conf", r.Main.Content)
	// Named from the rendered path rather than the input index: the render
	// also emits the ACME catch-all, so the two lists are not parallel.
	for _, f := range r.Vhosts {
		goldenCheck(t, "vhost_"+path.Base(path.Dir(f.Path))+".conf", f.Content)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	// Two renders of the same input must be byte-identical, otherwise Apply
	// would reload the webserver on every no-op change.
	b := newBackend(t)
	cfg := webserver.DefaultServerConfig()
	sites := []webserver.Site{
		testSite("a.example.com", "alice", webserver.AppPHP, "8.4"),
		testSite("b.example.com", "bob", webserver.AppStatic, ""),
	}
	first, err := b.Render(cfg, sites)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := b.Render(cfg, sites)
	if err != nil {
		t.Fatalf("render again: %v", err)
	}
	if string(first.Main.Content) != string(second.Main.Content) {
		t.Fatal("main config is not deterministic")
	}
}

func TestRenderNoSites(t *testing.T) {
	// A host with no sites must still get a valid config with a listener,
	// otherwise deleting the last site would leave an unparseable file.
	b := newBackend(t)
	r, err := b.Render(webserver.DefaultServerConfig(), nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// Only the ACME catch-all: a host with no sites must still be able to
	// obtain a certificate for its first one.
	if len(r.Vhosts) != 1 {
		t.Fatalf("got %d vhosts for an empty site list, want just the ACME catch-all", len(r.Vhosts))
	}
	main := string(r.Main.Content)
	if !strings.Contains(main, "listener opanel_http") {
		t.Fatal("the http listener is missing from an empty configuration")
	}
	if strings.Contains(main, "listener opanel_https") {
		t.Fatal("an https listener was emitted with no SSL site to serve")
	}
}

func TestRenderRejectsDuplicateHostname(t *testing.T) {
	b := newBackend(t)
	a := testSite("dup.example.com", "alice", webserver.AppPHP, "8.4")
	c := testSite("other.example.com", "bob", webserver.AppPHP, "8.4")
	c.Aliases = []string{"dup.example.com"} // collides with a's primary

	_, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{a, c})
	if err == nil {
		t.Fatal("two sites claiming one hostname were accepted")
	}
	if !strings.Contains(err.Error(), "dup.example.com") {
		t.Fatalf("error does not name the clashing hostname: %v", err)
	}
}

func TestRenderRejectsInvalidSite(t *testing.T) {
	b := newBackend(t)
	cfg := webserver.DefaultServerConfig()

	cases := map[string]func(*webserver.Site){
		"bad domain":           func(s *webserver.Site) { s.Domain = "not a domain" },
		"relative docroot":     func(s *webserver.Site) { s.DocumentRoot = "public_html" },
		"docroot escapes root": func(s *webserver.Site) { s.DocumentRoot = "/etc" },
		"traversal in docroot": func(s *webserver.Site) { s.DocumentRoot = s.VhostRoot + "/../../etc" },
		"brace in docroot":     func(s *webserver.Site) { s.DocumentRoot = s.VhostRoot + "/a}b" },
		"newline in docroot":   func(s *webserver.Site) { s.DocumentRoot = s.VhostRoot + "/a\nb" },
		"bad owner":            func(s *webserver.Site) { s.OwnerUser = "root; rm -rf /" },
		"unknown app type":     func(s *webserver.Site) { s.AppType = "ruby" },
		"unknown rewrite mode": func(s *webserver.Site) { s.RewriteMode = "magic" },
		"php site without php": func(s *webserver.Site) { s.LSAPIBinary = "" },
		"alias equals primary": func(s *webserver.Site) { s.Aliases = []string{s.Domain} },
		"ssl without cert":     func(s *webserver.Site) { s.SSLEnabled = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := testSite("valid.example.com", "alice", webserver.AppPHP, "8.4")
			mutate(&s)
			if _, err := b.Render(cfg, []webserver.Site{s}); err == nil {
				t.Fatal("invalid site was rendered")
			}
		})
	}
}

func TestStaticSiteHasNoInterpreter(t *testing.T) {
	b := newBackend(t)
	s := testSite("static.example.com", "alice", webserver.AppStatic, "")
	r, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(siteVhost(t, r, s.Domain))
	for _, forbidden := range []string{"extprocessor", "scripthandler", "lsapi"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("static site config contains %q", forbidden)
		}
	}
}

func TestExtAppNameCarriesTheVersion(t *testing.T) {
	// Regression: with a version-independent name the socket never changes,
	// so switching PHP version leaves the old interpreter serving the site.
	b := newBackend(t)
	s := testSite("example.com", "alice", webserver.AppPHP, "8.4")

	r84, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s})
	if err != nil {
		t.Fatalf("render 8.4: %v", err)
	}
	s.PHPVersion = "8.1"
	s.LSAPIBinary = "/usr/local/lsws/lsphp81/bin/lsphp"
	r81, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s})
	if err != nil {
		t.Fatalf("render 8.1: %v", err)
	}

	if !strings.Contains(string(siteVhost(t, r84, "example.com")), "uds://tmp/lshttpd/lsphp84_example_com.sock") {
		t.Error("8.4 socket path does not carry the version")
	}
	if !strings.Contains(string(siteVhost(t, r81, "example.com")), "uds://tmp/lshttpd/lsphp81_example_com.sock") {
		t.Error("8.1 socket path does not carry the version")
	}
}

func TestPHPSiteRunsAsItsOwner(t *testing.T) {
	// Per-site suEXEC is the whole reason each site gets its own external
	// processor; losing extUser would silently run every site as one account.
	b := newBackend(t)
	s := testSite("example.com", "alice", webserver.AppPHP, "8.4")
	r, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(siteVhost(t, r, s.Domain))
	for _, want := range []string{
		"extUser                   alice",
		"extGroup                  alice",
		"path                      /usr/local/lsws/lsphp84/bin/lsphp",
		"add                       lsapi:lsphp84_example_com php",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("vhost config is missing %q", want)
		}
	}
}

func TestSuspendedSiteServesOnlyTheNotice(t *testing.T) {
	// A suspended site must run none of the customer's code and must not
	// expose their document root, while keeping the vhost so that lifting
	// the suspension is a single re-render.
	b := newBackend(t)
	s := testSite("example.com", "alice", webserver.AppWordPress, "8.4")
	s.Suspended = true

	r, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(siteVhost(t, r, s.Domain))

	if !strings.Contains(body, "docRoot                       "+webserver.SuspendedRoot) {
		t.Error("document root was not swapped for the suspension notice")
	}
	for _, forbidden := range []string{"extprocessor", "scripthandler", s.DocumentRoot} {
		if strings.Contains(body, forbidden) {
			t.Errorf("suspended site config still contains %q", forbidden)
		}
	}
	// The vhost must still be mapped, or unsuspending would need a listener
	// change rather than just a re-render.
	if !strings.Contains(string(r.Main.Content), "map                          example.com example.com") {
		t.Error("suspended site is no longer mapped on the listener")
	}
}

func TestMainConfigMapsEveryHostname(t *testing.T) {
	b := newBackend(t)
	s := testSite("example.com", "alice", webserver.AppPHP, "8.4")
	s.Aliases = []string{"www.example.com", "shop.example.com"}
	r, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	main := string(r.Main.Content)
	want := "map                          example.com example.com, www.example.com, shop.example.com"
	if !strings.Contains(main, want) {
		t.Errorf("listener map is wrong; want %q in:\n%s", want, main)
	}
}

func TestSSLListenerOnlyWithSSLSites(t *testing.T) {
	b := newBackend(t)
	plain := testSite("plain.example.com", "alice", webserver.AppPHP, "8.4")
	secure := testSite("secure.example.com", "bob", webserver.AppPHP, "8.4")
	secure.SSLEnabled = true
	secure.CertFile = "/var/lib/opanel/ssl/secure.example.com/fullchain.pem"
	secure.KeyFile = "/var/lib/opanel/ssl/secure.example.com/privkey.pem"

	r, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{plain, secure})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	main := string(r.Main.Content)
	if !strings.Contains(main, "listener opanel_https") {
		t.Fatal("https listener missing")
	}
	// Only the SSL site may be mapped on the secure listener.
	https := main[strings.Index(main, "listener opanel_https"):]
	if strings.Contains(https, "plain.example.com") {
		t.Error("a plain site was mapped onto the https listener")
	}
	if !strings.Contains(https, "secure.example.com") {
		t.Error("the ssl site is not mapped onto the https listener")
	}
}

func TestVhostPathsAreUnderManagedDir(t *testing.T) {
	b := newBackend(t)
	s := testSite("example.com", "alice", webserver.AppPHP, "8.4")
	r, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{s})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := ManagedDir + "/vhosts/example.com/vhconf.conf"
	var found bool
	for _, f := range r.Vhosts {
		if f.Path == want {
			found = true
		}
	}
	if !found {
		t.Errorf("no vhost rendered at %q", want)
	}
	// The main config references it relative to the server root.
	if !strings.Contains(string(r.Main.Content), "conf/opanel/vhosts/example.com/vhconf.conf") {
		t.Error("main config does not reference the vhost file relatively")
	}
}

func TestRewriteRules(t *testing.T) {
	if got := rewriteRules(webserver.RewriteNone); got != "" {
		t.Errorf("rewrite mode none produced rules: %q", got)
	}
	for _, mode := range []string{webserver.RewriteLaravel, webserver.RewriteFrontController, webserver.RewriteCodeIgniter} {
		if !strings.Contains(rewriteRules(mode), "index.php") {
			t.Errorf("rewrite mode %q does not route to index.php", mode)
		}
	}
}

// TestWAFRendersOnlyWhenBothSidesAgree covers the two conditions that have to
// hold before ModSecurity appears in the configuration: the host has the
// engine, and the site asked for it. Naming a module the webserver does not
// have stops it starting, so getting this wrong takes every site down.
func TestWAFRendersOnlyWhenBothSidesAgree(t *testing.T) {
	b := newBackend(t)

	site := testSite("waf.example.com", "alice", webserver.AppPHP, "8.4")
	site.WAFEnabled = true

	off := testSite("plain.example.com", "bob", webserver.AppPHP, "8.4")

	cases := []struct {
		name        string
		rulesFile   string
		wantServer  bool
		wantOnSite  bool
		wantOnPlain bool
	}{
		{
			name:      "engine missing, so nothing is emitted even though the site asked",
			rulesFile: "",
		},
		{
			name:       "engine present and the site asked",
			rulesFile:  "/usr/local/lsws/conf/opanel/waf/rules.conf",
			wantServer: true,
			wantOnSite: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := webserver.DefaultServerConfig()
			cfg.WAFRulesFile = tc.rulesFile

			r, err := b.Render(cfg, []webserver.Site{site, off})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			main := string(r.Main.Content)
			if got := strings.Contains(main, "module mod_security"); got != tc.wantServer {
				t.Errorf("server block has the module = %v, want %v", got, tc.wantServer)
			}
			if tc.wantServer && !strings.Contains(main, tc.rulesFile) {
				t.Error("the server block does not name the rules file")
			}

			for _, f := range r.Vhosts {
				body := string(f.Content)
				on := strings.Contains(body, "modsecurity              on")
				switch {
				case strings.Contains(f.Path, "waf.example.com"):
					if on != tc.wantOnSite {
						t.Errorf("the protected site has the module = %v, want %v", on, tc.wantOnSite)
					}
				case strings.Contains(f.Path, "plain.example.com"):
					if on != tc.wantOnPlain {
						t.Errorf("the unprotected site has the module = %v, want %v", on, tc.wantOnPlain)
					}
				}
			}
		})
	}
}
