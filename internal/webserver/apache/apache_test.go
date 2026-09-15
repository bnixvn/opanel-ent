package apache

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnixvn/opanel-ent/internal/webserver"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Golden files are the safety net for the renderer. httpd -t catches an
// unknown directive, which OpenLiteSpeed's test does not, but it cannot tell
// the difference between a vhost that denies what it should and one that
// denies nothing -- and that difference is the whole point of some of these
// blocks. Comparing exact bytes is what catches a rule that quietly stopped
// being emitted.

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
		s.FPMSocket = "/run/opanel/fpm/" + domain + "-" + strings.ReplaceAll(phpVersion, ".", "") + ".sock"
	}
	return s
}

func goldenCheck(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden %s (run: go test ./internal/webserver/apache -update): %v", p, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from golden.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func siteVhost(t *testing.T, r webserver.Rendered, domain string) []byte {
	t.Helper()
	want := VhostsDir + "/" + domain + ".conf"
	for _, f := range r.Vhosts {
		if f.Path == want {
			return f.Content
		}
	}
	t.Fatalf("no vhost rendered for %q", domain)
	return nil
}

func render(t *testing.T, cfg webserver.ServerConfig, sites []webserver.Site) webserver.Rendered {
	t.Helper()
	b, err := New()
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	r, err := b.Render(cfg, sites)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return r
}

func TestRenderMatrix(t *testing.T) {
	cfg := webserver.DefaultServerConfig()

	static := testSite("static.example.org", "alice", webserver.AppStatic, "")

	php := testSite("example.com", "bob", webserver.AppPHP, "8.3")
	php.Aliases = []string{"www.example.com"}
	php.RewriteMode = webserver.RewriteLaravel
	php.PHPSettings = map[string]string{"memory_limit": "256M"}

	secure := testSite("secure.example.com", "carol", webserver.AppWordPress, "8.4")
	secure.SSLEnabled = true
	secure.ForceHTTPS = true
	secure.CertFile = "/var/lib/opanel/ssl/secure.example.com/fullchain.pem"
	secure.KeyFile = "/var/lib/opanel/ssl/secure.example.com/privkey.pem"

	legacy := testSite("legacy.example.net", "dave", webserver.AppPHP, "7.4")

	suspended := testSite("suspended.example.com", "erin", webserver.AppPHP, "8.3")
	suspended.Suspended = true

	r := render(t, cfg, []webserver.Site{static, php, secure, legacy, suspended})

	goldenCheck(t, "opanel.conf", r.Main.Content)
	goldenCheck(t, "vhost__default.conf", siteVhostFile(t, r, DefaultVhostFile))
	for _, s := range []webserver.Site{static, php, secure, legacy, suspended} {
		goldenCheck(t, "vhost_"+s.Domain+".conf", siteVhost(t, r, s.Domain))
	}
}

func siteVhostFile(t *testing.T, r webserver.Rendered, name string) []byte {
	t.Helper()
	for _, f := range r.Vhosts {
		if filepath.Base(f.Path) == name {
			return f.Content
		}
	}
	t.Fatalf("no file named %q in render", name)
	return nil
}

// A PHP site must reach an interpreter. The failure this guards against is
// not a broken render but a silent one: a vhost with no handler serves the
// source of every .php file it is asked for.
func TestPHPSiteAlwaysGetsAHandler(t *testing.T) {
	s := testSite("example.com", "bob", webserver.AppPHP, "8.3")
	r := render(t, webserver.DefaultServerConfig(), []webserver.Site{s})
	if !strings.Contains(string(siteVhost(t, r, s.Domain)), "SetHandler \"proxy:unix:"+s.FPMSocket) {
		t.Error("a PHP site rendered without a handler; it would serve its own source")
	}
}

func TestRefusesDuplicateHostname(t *testing.T) {
	a := testSite("example.com", "bob", webserver.AppPHP, "8.3")
	b2 := testSite("other.example.com", "carol", webserver.AppPHP, "8.3")
	b2.Aliases = []string{"example.com"}
	b, err := New()
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if _, err := b.Render(webserver.DefaultServerConfig(), []webserver.Site{a, b2}); err == nil {
		t.Fatal("two sites claimed one hostname and the render allowed it")
	}
}

// A suspended site must not keep serving the customer's application, and must
// keep answering the challenge path so its certificate can still renew.
func TestSuspendedServesNoticeButStillRenews(t *testing.T) {
	s := testSite("suspended.example.com", "erin", webserver.AppPHP, "8.3")
	s.Suspended = true
	r := render(t, webserver.DefaultServerConfig(), []webserver.Site{s})
	body := string(siteVhost(t, r, s.Domain))
	if strings.Contains(body, "SetHandler") {
		t.Error("a suspended site still has a PHP handler")
	}
	if !strings.Contains(body, webserver.SuspendedRoot) {
		t.Error("a suspended site does not serve the notice page")
	}
	if !strings.Contains(body, "acme-challenge") {
		t.Error("a suspended site cannot renew its certificate")
	}
}

// The panel declares a listener only where the distribution has not. A second
// Listen for a bound port stops Apache from starting at all.
func TestListenOnlyForNonStandardPorts(t *testing.T) {
	cfg := webserver.DefaultServerConfig()
	r := render(t, cfg, nil)
	if strings.Contains(string(r.Main.Content), "\nListen ") {
		t.Error("declared a listener for a port httpd.conf and ssl.conf already own")
	}

	cfg.HTTPPort, cfg.HTTPSPort = 8080, 8443
	r = render(t, cfg, nil)
	main := string(r.Main.Content)
	if !strings.Contains(main, "Listen 8080") || !strings.Contains(main, "Listen 8443") {
		t.Errorf("non-standard ports were not declared:\n%s", main)
	}
}
