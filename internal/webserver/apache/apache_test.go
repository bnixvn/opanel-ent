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

// The Manager's vhost is loopback-only, and that is the whole of its access
// control: the panel proxies to it after checking the session, and it does
// not check one of its own. A render that put it on a public listener would
// hand CloudLinux's administrative interface to the internet, and would still
// look right in every other respect -- so the test is about the address.
func TestCloudLinuxManagerVhostIsLoopbackOnly(t *testing.T) {
	cfg := webserver.DefaultServerConfig()
	cfg.LVERoot = "/usr/share/opanel/lvemanager"
	cfg.LVEPort = 8082
	cfg.LVEFPMSocket = "/run/opanel-fpm/lvemanager.sock"

	main := string(render(t, cfg, nil).Main.Content)

	if !strings.Contains(main, "Listen 127.0.0.1:8082") {
		t.Errorf("the Manager's listener was not bound to the loopback address:\n%s", main)
	}
	if strings.Contains(main, "\nListen 8082") {
		t.Errorf("the Manager was declared on a public listener:\n%s", main)
	}
	if !strings.Contains(main, "<VirtualHost 127.0.0.1:8082>") {
		t.Errorf("no vhost for the Manager:\n%s", main)
	}
	if !strings.Contains(main, "proxy:unix:/run/opanel-fpm/lvemanager.sock") {
		t.Errorf("the Manager's PHP was not sent to its pool:\n%s", main)
	}
	// Without this PHP sees a plain request and drops Secure from the cookies
	// it sets, on a connection the panel did in fact encrypt.
	if !strings.Contains(main, "HTTPS=on") {
		t.Errorf("the proxied scheme was not passed through:\n%s", main)
	}
}

// Nothing about the Manager is rendered when it is not installed. A vhost
// pointing at a directory that does not exist is a 500 waiting for somebody
// to find it, and an extra listener for no reason.
func TestNoManagerVhostWhenNotInstalled(t *testing.T) {
	main := string(render(t, webserver.DefaultServerConfig(), nil).Main.Content)
	if strings.Contains(main, "lvemanager") || strings.Contains(main, "8082") {
		t.Errorf("the Manager was rendered on a host that does not have it:\n%s", main)
	}
}

// Apache and LiteSpeed reach PHP by different routes, and the vhost carries
// both so that switching server starts a daemon rather than re-rendering the
// estate. The block has to be inside <IfModule LiteSpeed>: Apache has no such
// module and skips it, which is the only reason one file can serve both.
func TestPHPVhostCarriesBothInterpreters(t *testing.T) {
	site := testSite("both.example", "cust", webserver.AppPHP, "8.4")
	site.LSPHPHandler = "application/x-httpd-alt-php84"

	r := render(t, webserver.DefaultServerConfig(), []webserver.Site{site})
	out := string(siteVhost(t, r, site.Domain))

	if !strings.Contains(out, "SetHandler \"proxy:unix:") {
		t.Errorf("Apache lost its handler:\n%s", out)
	}
	i := strings.Index(out, "<IfModule LiteSpeed>")
	j := strings.Index(out, "AddHandler application/x-httpd-alt-php84 .php")
	k := strings.Index(out, "</IfModule>")
	if i < 0 || j < i || k < j {
		t.Errorf("the LiteSpeed handler is missing or outside its IfModule:\n%s", out)
	}
}

// A host with no alt-php for the site's version gets no block at all, rather
// than one naming an interpreter that is not there. Both are refusals, but
// this one is the panel's, made before the switch, with a message; the other
// is a 403 from LiteSpeed to the visitor.
func TestNoLiteSpeedBlockWithoutAnInterpreter(t *testing.T) {
	site := testSite("only-apache.example", "cust", webserver.AppPHP, "8.4")
	r := render(t, webserver.DefaultServerConfig(), []webserver.Site{site})
	out := string(siteVhost(t, r, site.Domain))
	if strings.Contains(out, "LiteSpeed") {
		t.Errorf("emitted a LiteSpeed handler for a version the host does not have:\n%s", out)
	}
}

// VhostDirs was declared and never set, so everything that asked a render
// where the vhosts live got an empty list and silently did nothing. The
// installer's guard against overwriting a live estate was one such caller,
// and it failed open: it decided a host with two sites had none.
func TestRenderReportsWhereVhostsLive(t *testing.T) {
	r := render(t, webserver.DefaultServerConfig(), nil)
	if len(r.VhostDirs) == 0 {
		t.Fatal("a render named no vhost directory; every caller that prunes or counts by it is a no-op")
	}
	for _, dir := range r.VhostDirs {
		if !strings.HasPrefix(dir, "/") {
			t.Errorf("vhost directory %q is not an absolute path", dir)
		}
	}
	// Every vhost the render produced has to be inside one of them, or Apply
	// would prune the file it just wrote.
	site := testSite("inside.example", "cust", webserver.AppPHP, "8.4")
	r = render(t, webserver.DefaultServerConfig(), []webserver.Site{site})
	for _, v := range r.Vhosts {
		ok := false
		for _, dir := range r.VhostDirs {
			if strings.HasPrefix(v.Path, dir+"/") {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s is not under any declared vhost directory %v", v.Path, r.VhostDirs)
		}
	}
}
