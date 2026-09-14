// Package ols renders and applies OpenLiteSpeed configuration.
//
// OpenLiteSpeed has no include directive -- tested on the target host, where
// an "include" line was accepted without complaint and then silently ignored,
// leaving the vhost unserved. So the panel owns httpd_config.conf in full and
// regenerates it from the database on every change. Per-site files live under
// a separate managed directory, which keeps the blast radius of one bad site
// to that site.
package ols

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path"
	"strings"
	"text/template"

	"github.com/bnixvn/opanel-ent/internal/phpini"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

//go:embed templates/*.tmpl
var templatesFS embed.FS

// Filesystem layout on the host.
const (
	ServerRoot     = "/usr/local/lsws"
	ConfDir        = ServerRoot + "/conf"
	MainConfigPath = ConfDir + "/httpd_config.conf"
	// ManagedDir holds everything the panel generates besides the main file.
	// Keeping it separate from conf/vhosts leaves anything an operator made
	// by hand untouched.
	ManagedDir = ConfDir + "/opanel"
	VhostsDir  = ManagedDir + "/vhosts"

	binOLS      = ServerRoot + "/bin/openlitespeed"
	binLSWSCtrl = ServerRoot + "/bin/lswsctrl"

	// ConfOwner is the account OpenLiteSpeed reads its configuration as.
	// Files written by the agent must end up owned by it or the server will
	// start with an empty configuration and no obvious reason why.
	ConfOwner = "lsadm"
	ConfGroup = "nobody"

	fileMode = 0o640
	dirMode  = 0o750
)

// BackupDir is where the previous configuration is kept for rollback.
const BackupDir = "/var/lib/opanel/webserver/last-good"

// Backend implements webserver.Backend for OpenLiteSpeed.
type Backend struct {
	serverRoot string
	confPath   string
	managedDir string
	backupDir  string
	tmpl       *template.Template
}

// New returns a backend using the standard paths.
func New() (*Backend, error) {
	t, err := template.New("ols").Funcs(template.FuncMap{
		"join": strings.Join,
	}).ParseFS(templatesFS, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("ols: parse templates: %w", err)
	}
	return &Backend{
		serverRoot: ServerRoot,
		confPath:   MainConfigPath,
		managedDir: ManagedDir,
		backupDir:  BackupDir,
		tmpl:       t,
	}, nil
}

// Name identifies the backend.
func (b *Backend) Name() string { return webserver.BackendOLS }

// ServiceUnit is the systemd unit that runs OpenLiteSpeed. The package also
// installs lsws.service and openlitespeed.service as aliases of this one.
func (b *Backend) ServiceUnit() string { return "lshttpd" }

// Installed reports whether OpenLiteSpeed is present.
func (b *Backend) Installed() bool {
	st, err := os.Stat(binOLS)
	return err == nil && !st.IsDir()
}

// siteView is what the templates see. Anything a template needs is computed
// here rather than in the template, so the logic is testable on its own.
type siteView struct {
	Site   webserver.Site
	Config webserver.ServerConfig

	Name          string // vhost block name
	ConfigFileRel string // configFile value, relative to the server root
	MapDomains    string // comma-separated hostnames for a listener map
	AppName       string // external processor name

	// EffectiveDocRoot is the document root actually served: the site's own,
	// or the shared suspension notice.
	EffectiveDocRoot string
	// ServePHP is NeedsPHP narrowed by suspension -- a suspended site gets no
	// interpreter at all.
	ServePHP bool
	// ServeWAF is the site's WAF flag narrowed by whether the host has the
	// engine. A vhost that switches on a module the server never loaded
	// stops OpenLiteSpeed starting, which takes down every site on the
	// machine rather than just this one.
	ServeWAF bool

	ACMEWebroot string

	// PHPIniLines are the site's php.ini overrides, already rendered and
	// sorted so the same settings always produce the same file.
	PHPIniLines []string

	MaxConns      int
	MemSoftLimit  string
	MemHardLimit  string
	ProcSoftLimit int
	ProcHardLimit int

	RewriteRules string
}

type mainView struct {
	Config          webserver.ServerConfig
	Sites           []siteView
	SSLSites        []siteView
	HasSSL          bool
	DefaultCertFile string
	DefaultKeyFile  string

	// WAFAvailable is whether the engine exists on this host. The server
	// block only loads it when it does; naming a module that is not there
	// stops OpenLiteSpeed starting.
	WAFAvailable bool
	WAFRulesFile string

	PMAEnabled     bool
	PMARoot        string
	PMAPort        int
	PMALSAPIBinary string

	ACMEVhostName     string
	ACMEWebroot       string
	ACMEConfigFileRel string
}

// acmeVhostConfPath is where the catch-all vhost's own config lives.
func (b *Backend) acmeVhostConfPath() string {
	return path.Join(b.managedDir, "vhosts", webserver.ACMEVhostName, "vhconf.conf")
}

// vhostName is the block name for a site.
//
// The domain is used verbatim: OpenLiteSpeed accepts dots in a block name,
// and a readable name is worth a great deal when reading a generated config
// during an incident.
func vhostName(domain string) string { return domain }

func (b *Backend) vhostConfPath(domain string) string {
	// path, not path/filepath: these are always paths on the Linux host, and
	// filepath would emit backslashes when the renderer runs on Windows
	// during development.
	return path.Join(b.managedDir, "vhosts", domain, "vhconf.conf")
}

// configFileRel converts an absolute path into the server-root-relative form
// OpenLiteSpeed expects in a configFile directive.
func (b *Backend) configFileRel(abs string) string {
	return strings.TrimPrefix(abs, b.serverRoot+"/")
}

// rewriteRules returns the rule body for a rewrite mode.
//
// These are a floor, not a ceiling: allowOverride is on, so an application's
// own .htaccess still applies. A site whose framework needs something else
// keeps working without the panel knowing about that framework.
func rewriteRules(mode string) string {
	switch mode {
	case webserver.RewriteFrontController, webserver.RewriteLaravel:
		return "    RewriteCond %{REQUEST_FILENAME} !-f\n" +
			"    RewriteCond %{REQUEST_FILENAME} !-d\n" +
			"    RewriteRule ^(.*)$ /index.php [L]"
	case webserver.RewriteCodeIgniter:
		return "    RewriteCond %{REQUEST_FILENAME} !-f\n" +
			"    RewriteCond %{REQUEST_FILENAME} !-d\n" +
			"    RewriteRule ^(.*)$ /index.php/$1 [L]"
	default:
		return ""
	}
}

// extAppName names a site's external processor.
//
// The PHP version is part of the name, and therefore part of the socket path,
// for a reason found the hard way: changing only the binary path leaves the
// already-running interpreter listening on the same socket, and a graceful
// reload happily keeps using it. The site then reports the old version
// forever. Varying the name makes the change a different application, so the
// old one is retired and a new one starts.
func extAppName(s webserver.Site) string {
	if !s.NeedsPHP() {
		return ""
	}
	slug := strings.NewReplacer(".", "_", "-", "_").Replace(s.Domain)
	return "lsphp" + strings.ReplaceAll(s.PHPVersion, ".", "") + "_" + slug
}

// phpLimits sizes one site's interpreter pool.
//
// Modest by design. Every site gets its own pool, so the ceiling that matters
// is the sum across sites, not any single one. Stage B replaces this with
// CloudLinux LVE, which caps per user rather than per site and does it in the
// kernel instead of by guessing here.
func phpLimits() (maxConns int, memSoft, memHard string, procSoft, procHard int) {
	return 10, "512M", "640M", 100, 120
}

func (b *Backend) newSiteView(cfg webserver.ServerConfig, s webserver.Site) siteView {
	maxConns, memSoft, memHard, procSoft, procHard := phpLimits()
	docRoot := s.DocumentRoot
	if s.Suspended {
		docRoot = webserver.SuspendedRoot
	}
	return siteView{
		EffectiveDocRoot: docRoot,
		ServePHP:         s.NeedsPHP() && !s.Suspended,
		// A suspended site is serving a notice page, so there is nothing to
		// protect and no reason to spend the engine on it.
		ServeWAF:      s.WAFEnabled && !s.Suspended && cfg.WAFRulesFile != "",
		ACMEWebroot:   webserver.ACMEWebroot,
		Site:          s,
		Config:        cfg,
		Name:          vhostName(s.Domain),
		ConfigFileRel: b.configFileRel(b.vhostConfPath(s.Domain)),
		MapDomains:    strings.Join(s.Hostnames(), ", "),
		AppName:       extAppName(s),
		MaxConns:      maxConns,
		MemSoftLimit:  memSoft,
		MemHardLimit:  memHard,
		ProcSoftLimit: procSoft,
		ProcHardLimit: procHard,
		RewriteRules:  rewriteRules(s.RewriteMode),
		// A suspended site runs no interpreter, so overrides for one would
		// be directives attached to nothing.
		PHPIniLines: phpIniLines(s),
	}
}

// phpIniLines renders the site's overrides, or nothing when the site is not
// running PHP at all.
func phpIniLines(s webserver.Site) []string {
	if !s.NeedsPHP() || s.Suspended || len(s.PHPSettings) == 0 {
		return nil
	}
	return phpini.Render(s.PHPSettings)
}

// Render produces the complete configuration. It performs no I/O, so it is
// covered by golden-file tests -- which matter more than they might appear to,
// because OpenLiteSpeed accepts an unknown directive with only a warning and
// TestConfig therefore cannot catch a misspelled one.
func (b *Backend) Render(cfg webserver.ServerConfig, sites []webserver.Site) (webserver.Rendered, error) {
	var out webserver.Rendered

	seen := make(map[string]string, len(sites)*2)
	views := make([]siteView, 0, len(sites))
	sslViews := make([]siteView, 0, len(sites))

	for _, s := range sites {
		if err := s.Validate(); err != nil {
			return out, fmt.Errorf("ols: site %q: %w", s.Domain, err)
		}
		// Two vhosts claiming the same hostname would make OpenLiteSpeed pick
		// one arbitrarily, so it is caught here rather than at reload.
		for _, h := range s.Hostnames() {
			if prev, dup := seen[h]; dup {
				return out, fmt.Errorf("ols: hostname %q is claimed by both %q and %q", h, prev, s.Domain)
			}
			seen[h] = s.Domain
		}

		v := b.newSiteView(cfg, s)
		views = append(views, v)
		if s.SSLEnabled {
			sslViews = append(sslViews, v)
		}
	}

	mv := mainView{
		Config: cfg, Sites: views, SSLSites: sslViews, HasSSL: len(sslViews) > 0,
		WAFAvailable: cfg.WAFRulesFile != "", WAFRulesFile: cfg.WAFRulesFile,
		PMAEnabled: cfg.PMARoot != "" && cfg.PMAPort > 0 && cfg.PMALSAPIBinary != "",
		PMARoot:    cfg.PMARoot, PMAPort: cfg.PMAPort,
		PMALSAPIBinary:    cfg.PMALSAPIBinary,
		ACMEVhostName:     webserver.ACMEVhostName,
		ACMEWebroot:       webserver.ACMEWebroot,
		ACMEConfigFileRel: b.configFileRel(b.acmeVhostConfPath()),
	}
	if mv.HasSSL {
		// OpenLiteSpeed needs a certificate on the listener itself; per-site
		// certificates are then selected by SNI from each vhssl block. Any
		// valid certificate serves as the listener default.
		mv.DefaultCertFile = sslViews[0].Site.CertFile
		mv.DefaultKeyFile = sslViews[0].Site.KeyFile
	}

	main, err := b.execute("httpd_config.conf.tmpl", mv)
	if err != nil {
		return out, err
	}
	out.Main = webserver.File{Path: b.confPath, Content: main, Mode: fileMode}

	// The catch-all is rendered like any other vhost so it is covered by the
	// same prune, ownership and rollback handling.
	acmeContent, err := b.execute("acme_vhconf.conf.tmpl", mv)
	if err != nil {
		return out, err
	}
	acmePath := b.acmeVhostConfPath()
	out.Vhosts = append(out.Vhosts, webserver.File{Path: acmePath, Content: acmeContent, Mode: fileMode})
	out.VhostDirs = append(out.VhostDirs, path.Dir(acmePath))

	for _, v := range views {
		content, err := b.execute("vhconf.conf.tmpl", v)
		if err != nil {
			return out, fmt.Errorf("ols: render vhost %q: %w", v.Site.Domain, err)
		}
		p := b.vhostConfPath(v.Site.Domain)
		out.Vhosts = append(out.Vhosts, webserver.File{Path: p, Content: content, Mode: fileMode})
		out.VhostDirs = append(out.VhostDirs, path.Dir(p))
	}
	return out, nil
}

func (b *Backend) execute(name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := b.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("ols: execute %s: %w", name, err)
	}
	return buf.Bytes(), nil
}
