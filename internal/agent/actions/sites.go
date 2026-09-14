package actions

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path"
	"strconv"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/acme"
	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/phpini"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// Certificate storage. The account key lives apart from the certificates so
// the two can have different permissions and different backup treatment.
const (
	CertStateDir = "/var/lib/opanel/acme"
	CertDir      = "/var/lib/opanel/ssl"
)

// --- Linux accounts -------------------------------------------------------

// AccountRequest names a site account.
type AccountRequest struct {
	Username string `json:"username"`
}

// Validate checks the account name against the panel's rules.
func (r *AccountRequest) Validate() error {
	if !linuxuser.ValidName(r.Username) {
		return fmt.Errorf("%q is not an acceptable account name", r.Username)
	}
	return nil
}

// AccountDeleteRequest asks for an account to be removed.
type AccountDeleteRequest struct {
	Username string `json:"username"`
	// RemoveHome must be set explicitly. Deleting a customer's files is not
	// something that should happen as a side effect of removing an account.
	RemoveHome bool `json:"remove_home"`
}

// Validate checks the account name.
func (r *AccountDeleteRequest) Validate() error {
	return (&AccountRequest{Username: r.Username}).Validate()
}

// SFTPCredentialRequest adds a second credential to an existing account.
type SFTPCredentialRequest struct {
	Username string `json:"username"`
	Owner    string `json:"owner"`
}

// Validate checks both names.
func (r *SFTPCredentialRequest) Validate() error {
	if err := (&AccountRequest{Username: r.Username}).Validate(); err != nil {
		return err
	}
	return (&AccountRequest{Username: r.Owner}).Validate()
}

// AccountPasswordRequest sets an account's SFTP password.
type AccountPasswordRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Validate checks the account name and that a password was supplied.
func (r *AccountPasswordRequest) Validate() error {
	if err := (&AccountRequest{Username: r.Username}).Validate(); err != nil {
		return err
	}
	if len(r.Password) < 12 {
		return fmt.Errorf("password must be at least 12 characters")
	}
	return nil
}

// --- Site files -----------------------------------------------------------

// SiteProvisionRequest creates the directory tree for one site.
type SiteProvisionRequest struct {
	Domain       string `json:"domain"`
	Owner        string `json:"owner"`
	VhostRoot    string `json:"vhost_root"`
	DocumentRoot string `json:"document_root"`
	AppType      string `json:"app_type"`
}

// Validate rejects anything that could place files outside the owner's home.
func (r *SiteProvisionRequest) Validate() error {
	if !webserver.ValidDomain(r.Domain) {
		return fmt.Errorf("domain %q is not a valid hostname", r.Domain)
	}
	if !linuxuser.ValidName(r.Owner) {
		return fmt.Errorf("owner %q is not an acceptable account name", r.Owner)
	}
	home := linuxuser.Home(r.Owner)
	// Containment is checked here rather than trusted from the caller: this
	// action runs as root, so an unchecked path would write anywhere.
	for _, p := range []struct{ name, value string }{
		{"vhost root", r.VhostRoot},
		{"document root", r.DocumentRoot},
	} {
		if p.value == "" || !path.IsAbs(p.value) || p.value != path.Clean(p.value) {
			return fmt.Errorf("%s %q must be a clean absolute path", p.name, p.value)
		}
		if p.value != home && !strings.HasPrefix(p.value, home+"/") {
			return fmt.Errorf("%s %q is outside %s", p.name, p.value, home)
		}
	}
	if r.DocumentRoot != r.VhostRoot && !strings.HasPrefix(r.DocumentRoot, r.VhostRoot+"/") {
		return fmt.Errorf("document root %q is outside vhost root %q", r.DocumentRoot, r.VhostRoot)
	}
	return nil
}

// SiteRemoveRequest deletes a site's files.
type SiteRemoveRequest struct {
	Domain    string `json:"domain"`
	Owner     string `json:"owner"`
	VhostRoot string `json:"vhost_root"`
}

// Validate applies the same containment rules as provisioning.
func (r *SiteRemoveRequest) Validate() error {
	return (&SiteProvisionRequest{
		Domain: r.Domain, Owner: r.Owner,
		VhostRoot: r.VhostRoot, DocumentRoot: r.VhostRoot,
	}).Validate()
}

// --- Webserver ------------------------------------------------------------

// SiteSpec is a site as the panel database knows it.
//
// Note what is absent: the PHP binary path. The caller sends "8.4" and the
// agent resolves it through whichever provider is active. That is what keeps
// the Stage B move to CloudLinux alt-php confined to the agent -- the API
// never learns where an interpreter lives.
type SiteSpec struct {
	Domain       string   `json:"domain"`
	Aliases      []string `json:"aliases,omitempty"`
	Owner        string   `json:"owner"`
	VhostRoot    string   `json:"vhost_root"`
	DocumentRoot string   `json:"document_root"`
	AppType      string   `json:"app_type"`
	PHPVersion   string   `json:"php_version,omitempty"`
	RewriteMode  string   `json:"rewrite_mode"`
	SSLEnabled   bool     `json:"ssl_enabled"`
	CertFile     string   `json:"cert_file,omitempty"`
	KeyFile      string   `json:"key_file,omitempty"`
	ForceHTTPS   bool     `json:"force_https"`
	WAFEnabled   bool     `json:"waf_enabled"`
	Suspended    bool     `json:"suspended"`
	// PHPSettings are php.ini overrides for this site. The agent validates
	// them again rather than trusting the panel: this is the process that
	// writes the webserver's configuration, so it is the last place a bad
	// value can be stopped.
	PHPSettings map[string]string `json:"php_settings,omitempty"`
}

// WebserverApplyRequest carries the complete desired state.
//
// Every site is sent on every change. That is deliberate: the renderer
// regenerates the whole configuration, which is what makes switching backends
// and recovering from a partial write safe.
type WebserverApplyRequest struct {
	Config webserver.ServerConfig `json:"config"`
	Sites  []SiteSpec             `json:"sites"`
}

// Validate performs the checks that do not need a provider. Full validation
// happens in resolve, once the interpreter path is known.
func (r *WebserverApplyRequest) Validate() error {
	if r.Config.HTTPPort <= 0 || r.Config.HTTPPort > 65535 {
		return fmt.Errorf("http port %d is out of range", r.Config.HTTPPort)
	}
	for _, s := range r.Sites {
		if s.PHPVersion != "" && !phpmgr.ValidVersion(s.PHPVersion) {
			return fmt.Errorf("site %q: php version %q is malformed", s.Domain, s.PHPVersion)
		}
	}
	return nil
}

// resolve turns specs into renderable sites, filling in interpreter paths.
func resolve(p phpmgr.Provider, specs []SiteSpec) ([]webserver.Site, error) {
	out := make([]webserver.Site, 0, len(specs))
	for _, sp := range specs {
		s := webserver.Site{
			Domain:       sp.Domain,
			Aliases:      sp.Aliases,
			VhostRoot:    sp.VhostRoot,
			DocumentRoot: sp.DocumentRoot,
			AppType:      sp.AppType,
			RewriteMode:  sp.RewriteMode,
			OwnerUser:    sp.Owner,
			OwnerGroup:   sp.Owner,
			PHPVersion:   sp.PHPVersion,
			SSLEnabled:   sp.SSLEnabled,
			CertFile:     sp.CertFile,
			KeyFile:      sp.KeyFile,
			ForceHTTPS:   sp.ForceHTTPS,
			WAFEnabled:   sp.WAFEnabled,
			Suspended:    sp.Suspended,
		}
		if len(sp.PHPSettings) > 0 {
			checked, err := phpini.Check(sp.PHPSettings)
			if err != nil {
				return nil, fmt.Errorf("site %q: php settings: %w", sp.Domain, err)
			}
			s.PHPSettings = checked
		}
		if s.NeedsPHP() {
			s.LSAPIBinary = p.LSAPIBinary(sp.PHPVersion)
		}
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("site %q: %w", sp.Domain, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// WebserverStatusResult reports which backend is active and healthy.
type WebserverStatusResult struct {
	Backend   string `json:"backend"`
	Unit      string `json:"unit"`
	Installed bool   `json:"installed"`
}

// --- PHP ------------------------------------------------------------------

// PHPVersionRequest names a PHP version.
type PHPVersionRequest struct {
	Version string `json:"version"`
}

// Validate checks the version shape.
func (r *PHPVersionRequest) Validate() error {
	if !phpmgr.ValidVersion(r.Version) {
		return fmt.Errorf("php version %q is malformed", r.Version)
	}
	return nil
}

// PHPListResult reports every supported version and whether it is installed.
type PHPListResult struct {
	Provider string           `json:"provider"`
	Versions []phpmgr.Version `json:"versions"`
}

// --- certificates ---------------------------------------------------------

// CertIssueRequest asks for a certificate covering one or more hostnames.
type CertIssueRequest struct {
	Domains []string `json:"domains"`
	Email   string   `json:"email"`
	// Staging targets Let's Encrypt's test CA. The result is not trusted by
	// browsers, but production rate limits are per registered domain per week
	// and a debugging loop will exhaust them.
	Staging bool `json:"staging"`
}

// Validate checks the request shape.
func (r *CertIssueRequest) Validate() error {
	if len(r.Domains) == 0 {
		return fmt.Errorf("at least one domain is required")
	}
	for _, d := range r.Domains {
		if !webserver.ValidDomain(d) {
			return fmt.Errorf("domain %q is not a valid hostname", d)
		}
	}
	// An empty contact is allowed by ACME and by this action. It costs the
	// expiry warnings Let's Encrypt would otherwise send, so the caller is
	// expected to know what it is giving up.
	return nil
}

// --- registration ---------------------------------------------------------

func registerSites(r *agent.Registry, deps Deps) {
	agent.Register(r, "linuxuser.create", 1, func(ctx context.Context, in AccountRequest) (linuxuser.Account, error) {
		acct, err := linuxuser.Create(ctx, in.Username)
		if err != nil {
			return linuxuser.Account{}, err
		}
		return *acct, nil
	})

	agent.Register(r, "linuxuser.create_sftp", 1, func(ctx context.Context, in SFTPCredentialRequest) (linuxuser.Account, error) {
		acct, err := linuxuser.CreateSFTP(ctx, in.Username, in.Owner)
		if err != nil {
			return linuxuser.Account{}, err
		}
		return *acct, nil
	})

	agent.Register(r, "linuxuser.delete", 1, func(ctx context.Context, in AccountDeleteRequest) (struct{}, error) {
		return struct{}{}, linuxuser.Delete(ctx, in.Username, in.RemoveHome)
	})

	agent.Register(r, "linuxuser.set_password", 1, func(ctx context.Context, in AccountPasswordRequest) (struct{}, error) {
		return struct{}{}, linuxuser.SetPassword(ctx, in.Username, in.Password)
	})

	agent.Register(r, "site.provision", 1, func(ctx context.Context, in SiteProvisionRequest) (struct{}, error) {
		return struct{}{}, provisionSite(ctx, in)
	})

	agent.Register(r, "site.remove", 1, func(_ context.Context, in SiteRemoveRequest) (struct{}, error) {
		if err := os.RemoveAll(in.VhostRoot); err != nil {
			return struct{}{}, fmt.Errorf("remove %s: %w", in.VhostRoot, err)
		}
		return struct{}{}, nil
	})

	agent.Register(r, "webserver.apply", 1, func(ctx context.Context, in WebserverApplyRequest) (struct{}, error) {
		sites, err := resolve(deps.PHP, in.Sites)
		if err != nil {
			return struct{}{}, &agent.PayloadError{Err: err}
		}
		// Sites created before the log directory mode was corrected still
		// have one the webserver cannot enter. Fixing it here means the
		// existing estate is repaired by the next configuration change
		// rather than needing every site touched by hand.
		for _, site := range sites {
			logs := path.Join(site.VhostRoot, "logs")
			if info, err := os.Stat(logs); err == nil && info.Mode().Perm() == 0o750 {
				if err := os.Chmod(logs, 0o751); err != nil {
					return struct{}{}, fmt.Errorf("repair %s: %w", logs, err)
				}
			}
		}
		// The agent decides whether the WAF is loadable, because it is the
		// side that can see the files. The API asks for the configuration it
		// wants; the host says what it actually has.
		cfg := in.Config
		cfg.WAFRulesFile = wafRulesIfInstalled()
		if st := pmaStatus(); st.Installed {
			cfg.PMARoot = st.Root
			cfg.PMAPort = PMAPort
			cfg.PMALSAPIBinary = deps.PHP.LSAPIBinary(PMAPHPVersion)
		}

		rendered, err := deps.Webserver.Render(cfg, sites)
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, deps.Webserver.Apply(ctx, rendered)
	})

	agent.Register(r, "webserver.test", 1, func(ctx context.Context, _ struct{}) (struct{}, error) {
		return struct{}{}, deps.Webserver.TestConfig(ctx)
	})

	agent.Register(r, "webserver.status", 1, func(_ context.Context, _ struct{}) (WebserverStatusResult, error) {
		return WebserverStatusResult{
			Backend:   deps.Webserver.Name(),
			Unit:      deps.Webserver.ServiceUnit(),
			Installed: deps.Webserver.Installed(),
		}, nil
	})

	agent.Register(r, "php.list", 1, func(ctx context.Context, _ struct{}) (PHPListResult, error) {
		vs, err := deps.PHP.Available(ctx)
		return PHPListResult{Provider: deps.PHP.Name(), Versions: vs}, err
	})

	agent.Register(r, "php.install", 1, func(ctx context.Context, in PHPVersionRequest) (struct{}, error) {
		return struct{}{}, deps.PHP.Install(ctx, in.Version)
	})

	agent.Register(r, "php.uninstall", 1, func(ctx context.Context, in PHPVersionRequest) (struct{}, error) {
		return struct{}{}, deps.PHP.Uninstall(ctx, in.Version)
	})

	agent.Register(r, "cert.issue", 1, func(ctx context.Context, in CertIssueRequest) (acme.Certificate, error) {
		m := &acme.Manager{
			StateDir: CertStateDir,
			CertDir:  CertDir,
			Webroot:  webserver.ACMEWebroot,
			Email:    in.Email,
			CADirURL: acme.ProductionCA,
		}
		if in.Staging {
			m.CADirURL = acme.StagingCA
		}
		cert, err := m.Issue(ctx, in.Domains)
		if err != nil {
			return acme.Certificate{}, err
		}
		// The agent writes these as root, but the API reads its own
		// certificate as the unprivileged service account. Without this the
		// panel starts, fails to load the key, and reports nothing more
		// helpful than a permission error at boot.
		if err := grantPanelAccess(cert.CertFile, cert.KeyFile); err != nil {
			return acme.Certificate{}, err
		}
		return *cert, nil
	})
}

// PanelServiceUser is the account opanel-api runs as.
const PanelServiceUser = "opanel"

// grantPanelAccess lets the unprivileged API read a certificate it did not
// write. Group-readable rather than world: the private key stays off limits
// to site owners, who share the host.
func grantPanelAccess(certFile, keyFile string) error {
	u, err := user.Lookup(PanelServiceUser)
	if err != nil {
		return fmt.Errorf("look up %q: %w", PanelServiceUser, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	for _, p := range []struct {
		path string
		mode os.FileMode
	}{
		{path.Dir(certFile), 0o750},
		{certFile, 0o644},
		{keyFile, 0o640},
	} {
		if err := os.Chown(p.path, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", p.path, err)
		}
		if err := os.Chmod(p.path, p.mode); err != nil {
			return fmt.Errorf("chmod %s: %w", p.path, err)
		}
	}
	return nil
}

// provisionSite creates the directory tree and a placeholder page.
func provisionSite(ctx context.Context, in SiteProvisionRequest) error {
	acct, err := linuxuser.Lookup(in.Owner)
	if err != nil {
		return err
	}
	if acct == nil {
		return fmt.Errorf("owner %q has no Linux account", in.Owner)
	}
	uid, gid := int(acct.UID), int(acct.GID)

	// Modes here are load-bearing and were wrong on the first attempt, which
	// produced a 403 on every request:
	//
	//   0711 on the site root  -- the webserver worker runs as its own account
	//                             (nobody), not as the site owner, so it must
	//                             be able to traverse in. It has no business
	//                             listing the directory, hence execute only.
	//   0751 on logs           -- 0750 was wrong and cost every site its
	//                             access log. The webserver opens the log as
	//                             its own account, so it has to traverse in;
	//                             with 0750 it could not, wrote nothing, and
	//                             reported no error. Execute-only for other
	//                             accounts still keeps the directory
	//                             unlistable, and the log files themselves
	//                             decide who may read them.
	//   0755 on the doc root   -- this is what the world is meant to reach.
	dirs := []struct {
		path string
		mode os.FileMode
	}{
		{in.VhostRoot, 0o711},
		{path.Join(in.VhostRoot, "logs"), 0o751},
		{in.DocumentRoot, 0o755},
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return fmt.Errorf("create %s: %w", d.path, err)
		}
		if err := os.Chown(d.path, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", d.path, err)
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return fmt.Errorf("chmod %s: %w", d.path, err)
		}
	}

	index := path.Join(in.DocumentRoot, "index.html")
	if _, err := os.Stat(index); os.IsNotExist(err) {
		if err := writeOwned(index, placeholderPage(in.Domain), 0o644, uid, gid); err != nil {
			return err
		}
	}
	_ = ctx
	return nil
}

func writeOwned(p string, content []byte, mode os.FileMode, uid, gid int) error {
	if err := os.WriteFile(p, content, mode); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := os.Chown(p, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", p, err)
	}
	return nil
}

func placeholderPage(domain string) []byte {
	return []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + domain + `</title>
<style>
  body{font:16px/1.6 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;
       margin:0;min-height:100vh;display:grid;place-items:center;
       background:#fafafa;color:#1a1a1a}
  main{max-width:32rem;padding:2rem;text-align:center}
  h1{font-size:1.35rem;margin:0 0 .5rem;font-weight:600}
  p{margin:0;color:#666}
  code{background:#eee;padding:.15em .4em;border-radius:3px;font-size:.9em}
</style>
</head>
<body>
<main>
  <h1>` + domain + ` is ready</h1>
  <p>Upload your site to <code>public_html</code> to replace this page.</p>
</main>
</body>
</html>
`)
}

// wafRulesIfInstalled returns the ModSecurity configuration path when both
// the engine and its rules are present, and "" otherwise.
//
// Naming a module the webserver does not have stops it starting, so an
// operator who has not installed the rules gets a working server rather than
// a broken one.
func wafRulesIfInstalled() string {
	if _, err := os.Stat(modSecModule); err != nil {
		return ""
	}
	if _, err := os.Stat(wafRulesFile); err != nil {
		return ""
	}
	return wafRulesFile
}
