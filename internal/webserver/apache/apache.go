// Package apache renders and applies Apache httpd configuration.
//
// Apache is the default backend, and the reason is interoperability rather
// than preference. LiteSpeed Enterprise reads Apache's own configuration
// files, so one renderer serves both: switching between them starts a
// different daemon on the same configuration instead of migrating anything.
// That is what makes "switch web server" a ten-second operation that can be
// undone just as fast.
//
// Apache also has a real include directive, so the panel owns one file in
// conf.d and a directory of its own rather than the whole main config. An
// operator's hand-written conf.d file survives everything the panel does.
//
// PHP runs as PHP-FPM, one pool per site owned by that site's account, and
// Apache proxies to its unix socket. Pools are rendered by internal/phpfpm,
// not here: LiteSpeed Enterprise uses the same pools, and a renderer that
// emitted them would tie them to Apache.
package apache

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"text/template"

	"github.com/bnixvn/opanel-ent/internal/webserver"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

// Filesystem layout on the host.
const (
	ServerRoot = "/etc/httpd"
	// MainConfigPath is the panel's single file in the distribution's
	// include directory. It sorts before ssl.conf and welcome.conf, so the
	// vhosts it pulls in are the first Apache reads and therefore the ones
	// it serves for a hostname nothing else claims.
	MainConfigPath = ServerRoot + "/conf.d/opanel.conf"
	// ManagedDir holds everything else the panel generates. Nothing outside
	// it is ever written, and everything inside it is disposable.
	ManagedDir = ServerRoot + "/opanel"
	VhostsDir  = ManagedDir + "/vhosts"

	binHTTPD = "/usr/sbin/httpd"
	// binLSWS is LiteSpeed Enterprise, which reads the same files.
	binLSWS = "/usr/local/lsws/bin/lshttpd"
	// binOLS is OpenLiteSpeed, which installs to the same directory and so
	// is the one thing that makes binLSWS ambiguous.
	binOLS = "/usr/local/lsws/bin/openlitespeed"

	// DefaultVhostFile answers for hostnames no site claims. The numeric
	// prefix is load order, which for Apache is what decides the default.
	DefaultVhostFile = "00-default.conf"

	fileMode = 0o644
	dirMode  = 0o755
)

// BackupDir is where the previous configuration is kept for rollback.
const BackupDir = "/var/lib/opanel/webserver/apache-last-good"

// Backend implements webserver.Backend for Apache httpd.
type Backend struct {
	name string
	// httpPort is remembered from the last render, for the one setting that
	// is written outside the Apache configuration: LiteSpeed Enterprise has
	// to be told which port Apache was using. Nothing else reads it.
	httpPort   int
	confPath   string
	managedDir string
	backupDir  string
	unit       string
	tmpl       *template.Template
}

// New returns a backend that drives Apache.
func New() (*Backend, error) {
	return newFor(webserver.BackendApache, "httpd")
}

// NewLiteSpeed returns a backend that drives LiteSpeed Enterprise on the same
// configuration.
//
// This is not a shortcut. LiteSpeed Enterprise reads Apache's configuration
// files as its own -- that is the product's central claim and the reason it
// can replace Apache in place. So the two backends differ in exactly one
// thing, which daemon is running, and switching between them rewrites
// nothing. The LiteSpeed-specific tuning that does exist lives in
// /usr/local/lsws/conf and is written by the installer, not per site.
func NewLiteSpeed() (*Backend, error) {
	return newFor(webserver.BackendLSWS, "lshttpd")
}

func newFor(name, unit string) (*Backend, error) {
	t, err := template.New("apache").Funcs(template.FuncMap{
		"join": strings.Join,
	}).ParseFS(templatesFS, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("apache: parse templates: %w", err)
	}
	return &Backend{
		name:       name,
		confPath:   MainConfigPath,
		managedDir: ManagedDir,
		backupDir:  BackupDir,
		unit:       unit,
		tmpl:       t,
	}, nil
}

// Name identifies the backend.
func (b *Backend) Name() string { return b.name }

// ServiceUnit is the systemd unit that runs it.
func (b *Backend) ServiceUnit() string { return b.unit }

// Installed reports whether the daemon this backend drives is present.
//
// For LiteSpeed Enterprise that is not just "is the binary there". Both
// LiteSpeed servers install into /usr/local/lsws and both provide
// bin/lshttpd, so on a host with OpenLiteSpeed the binary exists and is the
// wrong server. The file only OpenLiteSpeed has is what tells them apart --
// and the fact that the two cannot coexist is why installing Enterprise
// refuses to run over the free one rather than overwriting it.
func (b *Backend) Installed() bool {
	if b.name == webserver.BackendLSWS {
		if isFile(binOLS) {
			return false
		}
		return isFile(binLSWS)
	}
	return isFile(binHTTPD)
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// mainData is what the global template needs.
type mainData struct {
	Config    webserver.ServerConfig
	VhostsDir string
	// ExtraListen carries a Listen directive only where the distribution has
	// not already declared one. httpd.conf declares 80 and ssl.conf declares
	// 443, and a second Listen for a port already bound stops Apache from
	// starting at all -- so the panel emits one only where there is nothing
	// to collide with.
	ExtraListen []string
	ACMEWebroot string
	Suspended   string
}

// vhostData is what the per-site template needs.
type vhostData struct {
	Site        webserver.Site
	Config      webserver.ServerConfig
	ACMEWebroot string
	Suspended   string
	// Rewrite is the block for the site's framework convention, already
	// chosen so the template holds no logic.
	Rewrite string
}

// Render produces the whole configuration. Pure: same inputs, same bytes.
func (b *Backend) Render(cfg webserver.ServerConfig, sites []webserver.Site) (webserver.Rendered, error) {
	for i := range sites {
		if err := sites[i].Validate(); err != nil {
			return webserver.Rendered{}, fmt.Errorf("apache: site %q: %w", sites[i].Domain, err)
		}
		if sites[i].LSAPIBinary != "" {
			return webserver.Rendered{}, fmt.Errorf(
				"apache: site %q was resolved for LiteSpeed's interpreter, not a php-fpm pool",
				sites[i].Domain)
		}
	}
	if err := checkDuplicates(sites); err != nil {
		return webserver.Rendered{}, err
	}

	b.httpPort = cfg.HTTPPort
	main, err := b.render("opanel.conf.tmpl", mainData{
		Config:      cfg,
		VhostsDir:   VhostsDir,
		ExtraListen: extraListen(cfg),
		ACMEWebroot: webserver.ACMEWebroot,
		Suspended:   webserver.SuspendedRoot,
	})
	if err != nil {
		return webserver.Rendered{}, err
	}

	out := webserver.Rendered{
		Main: webserver.File{Path: b.confPath, Content: main, Mode: fileMode},
	}

	def, err := b.render("default.conf.tmpl", mainData{
		Config:      cfg,
		ACMEWebroot: webserver.ACMEWebroot,
		Suspended:   webserver.SuspendedRoot,
	})
	if err != nil {
		return webserver.Rendered{}, err
	}
	out.Vhosts = append(out.Vhosts, webserver.File{
		Path: path.Join(VhostsDir, DefaultVhostFile), Content: def, Mode: fileMode,
	})

	for _, s := range sites {
		body, err := b.render("vhost.conf.tmpl", vhostData{
			Site:        s,
			Config:      cfg,
			ACMEWebroot: webserver.ACMEWebroot,
			Suspended:   webserver.SuspendedRoot,
			Rewrite:     rewriteBlock(s.RewriteMode),
		})
		if err != nil {
			return webserver.Rendered{}, err
		}
		out.Vhosts = append(out.Vhosts, webserver.File{
			Path: path.Join(VhostsDir, s.Domain+".conf"), Content: body, Mode: fileMode,
		})
	}
	return out, nil
}

func (b *Backend) render(name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := b.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("apache: render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// checkDuplicates refuses two sites claiming one hostname.
//
// Apache serves the first vhost that matches and says nothing about the
// second, so a duplicate is not an error at runtime -- it is one site quietly
// answering for another's domain, which is worth failing the render over.
func checkDuplicates(sites []webserver.Site) error {
	seen := make(map[string]string)
	for _, s := range sites {
		for _, h := range s.Hostnames() {
			if first, dup := seen[h]; dup {
				return fmt.Errorf("apache: %q is claimed by both %q and %q", h, first, s.Domain)
			}
			seen[h] = s.Domain
		}
	}
	return nil
}

// extraListen returns the listeners the panel must declare itself.
func extraListen(cfg webserver.ServerConfig) []string {
	var out []string
	if cfg.HTTPPort != 0 && cfg.HTTPPort != 80 {
		out = append(out, strconv.Itoa(cfg.HTTPPort))
	}
	if cfg.HTTPSPort != 0 && cfg.HTTPSPort != 443 {
		out = append(out, strconv.Itoa(cfg.HTTPSPort))
	}
	// phpMyAdmin is bound to the loopback address rather than to the port on
	// every interface: the panel reaches it through a proxy of its own, and
	// nothing else has any business reaching it at all.
	if cfg.PMARoot != "" && cfg.PMAPort != 0 {
		out = append(out, "127.0.0.1:"+strconv.Itoa(cfg.PMAPort))
	}
	return out
}

// rewriteBlock returns the rewrite rules for a framework convention.
//
// Every one of these is what the framework's own .htaccess would do, and
// .htaccess still works -- AllowOverride is All inside a site. They are
// emitted anyway because a customer who deletes .htaccess while deploying
// should get a working site back, not a directory listing of their source.
func rewriteBlock(mode string) string {
	switch mode {
	case webserver.RewriteFrontController, webserver.RewriteLaravel:
		// Laravel's public/index.php convention, which is also the generic
		// front-controller one: anything that is not a real file or
		// directory goes to index.php.
		return `        RewriteEngine On
        RewriteCond %{REQUEST_FILENAME} !-f
        RewriteCond %{REQUEST_FILENAME} !-d
        RewriteRule ^ index.php [L]`
	case webserver.RewriteCodeIgniter:
		// CodeIgniter wants the path in PATH_INFO rather than a bare
		// dispatch, which is the one difference from the block above.
		return `        RewriteEngine On
        RewriteCond %{REQUEST_FILENAME} !-f
        RewriteCond %{REQUEST_FILENAME} !-d
        RewriteRule ^(.*)$ index.php/$1 [L]`
	default:
		return ""
	}
}
