// Package sites orchestrates website lifecycle across the database, the
// Linux filesystem and the webserver.
//
// It runs inside opanel-api and holds no privilege of its own: every step
// that touches the host is an agent action. The ordering here is what makes
// the operations safe to retry.
package sites

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/acme"
	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// Service manages websites.
// Limits is the package check a site creation must pass. An interface rather
// than the concrete service, so this package does not import plans and the
// dependency stays one-directional.
type Limits interface {
	CheckSite(ctx context.Context, ownerID int64) error
}

type Service struct {
	db     *db.DB
	agent  *agentclient.Client
	log    *slog.Logger
	cfg    webserver.ServerConfig
	limits Limits
}

// New builds a Service.
func New(database *db.DB, ac *agentclient.Client, cfg webserver.ServerConfig,
	limits Limits, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: database, agent: ac, log: log, cfg: cfg, limits: limits}
}

// Errors callers are expected to branch on.
var (
	ErrDomainTaken      = errors.New("sites: domain already exists")
	ErrOwnerNotFound    = errors.New("sites: owner does not exist")
	ErrOwnerNotEligible = errors.New("sites: that account cannot own a website")
	ErrPHPNotReady      = errors.New("sites: requested PHP version is not installed")
)

// CreateRequest describes a new site.
type CreateRequest struct {
	Domain      string
	Aliases     []string
	OwnerID     int64
	AppType     string
	PHPVersion  string
	RewriteMode string
}

// Normalise trims and lowercases hostnames and fills in defaults, so the same
// site described two ways produces one record.
func (r *CreateRequest) Normalise() {
	r.Domain = strings.ToLower(strings.TrimSpace(r.Domain))
	for i := range r.Aliases {
		r.Aliases[i] = strings.ToLower(strings.TrimSpace(r.Aliases[i]))
	}
	r.Aliases = slices.Compact(slices.Sorted(slices.Values(r.Aliases)))
	r.Aliases = slices.DeleteFunc(r.Aliases, func(a string) bool { return a == "" || a == r.Domain })
	if r.RewriteMode == "" {
		r.RewriteMode = webserver.RewriteNone
	}
	if r.AppType == webserver.AppStatic {
		r.PHPVersion = ""
	}
}

// Validate checks a request without touching the database.
func (r CreateRequest) Validate() error {
	if !webserver.ValidDomain(r.Domain) {
		return fmt.Errorf("domain %q is not a valid hostname", r.Domain)
	}
	for _, a := range r.Aliases {
		if !webserver.ValidDomain(a) {
			return fmt.Errorf("alias %q is not a valid hostname", a)
		}
	}
	if !slices.Contains(webserver.AppTypes, r.AppType) {
		return fmt.Errorf("app type %q is not one of %v", r.AppType, webserver.AppTypes)
	}
	if !slices.Contains(webserver.RewriteModes, r.RewriteMode) {
		return fmt.Errorf("rewrite mode %q is not one of %v", r.RewriteMode, webserver.RewriteModes)
	}
	needsPHP := r.AppType == webserver.AppPHP || r.AppType == webserver.AppWordPress
	if needsPHP && !phpmgr.ValidVersion(r.PHPVersion) {
		return fmt.Errorf("a %s site needs a PHP version", r.AppType)
	}
	return nil
}

// Create provisions a site.
//
// Order matters and is chosen so a failure at any step leaves something an
// operator can retry rather than a half-built site:
//
//  1. the Linux account, which is idempotent;
//  2. the site row, which is what makes the domain unique;
//  3. the directory tree, also idempotent;
//  4. the webserver configuration, rendered from the database in full.
//
// If step 4 fails the row is removed again, because a site that exists in the
// database but is not served is the one state that would confuse everything
// downstream.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*db.Site, error) {
	req.Normalise()
	if err := req.Validate(); err != nil {
		return nil, err
	}

	owner, err := s.db.UserByID(ctx, req.OwnerID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrOwnerNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.SiteByDomain(ctx, req.Domain); err == nil {
		return nil, ErrDomainTaken
	} else if !errors.Is(err, db.ErrNotFound) {
		return nil, err
	}
	if err := s.checkHostnamesFree(ctx, req.Domain, req.Aliases, 0); err != nil {
		return nil, err
	}
	// Checked before anything is created, so a refusal leaves nothing behind.
	if s.limits != nil {
		if err := s.limits.CheckSite(ctx, owner.ID); err != nil {
			return nil, err
		}
	}
	if req.PHPVersion != "" {
		if err := s.requirePHP(ctx, req.PHPVersion); err != nil {
			return nil, err
		}
	}

	// A website lives inside its owner's home directory, so an owner who
	// cannot have one cannot own a website. Said here, before anything is
	// created, because the alternative was the agent refusing the name
	// "admin" several steps later with a message that explained nothing.
	if !linuxuser.ValidName(owner.Username) {
		return nil, fmt.Errorf(
			"%w: %q is a staff account with no home directory on the server. "+
				"A website has to belong to a hosting account, because its files live "+
				"in that account's home", ErrOwnerNotEligible, owner.Username)
	}
	if err := s.EnsureAccount(ctx, owner); err != nil {
		return nil, err
	}

	vhostRoot := path.Join(linuxuser.Home(owner.Username), req.Domain)
	site, err := s.db.CreateSite(ctx, &db.Site{
		Domain:       req.Domain,
		OwnerID:      owner.ID,
		AppType:      req.AppType,
		PHPVersion:   req.PHPVersion,
		DocumentRoot: path.Join(vhostRoot, "public_html"),
		Aliases:      req.Aliases,
		RewriteMode:  req.RewriteMode,
	})
	if err != nil {
		return nil, err
	}

	if _, err := agentclient.Call[struct{}](ctx, s.agent, "site.provision", 1,
		actions.SiteProvisionRequest{
			Domain:       site.Domain,
			Owner:        owner.Username,
			VhostRoot:    vhostRoot,
			DocumentRoot: site.DocumentRoot,
			AppType:      site.AppType,
		}); err != nil {
		s.rollbackRow(ctx, site.ID, "provision")
		return nil, fmt.Errorf("provision files: %w", err)
	}

	if err := s.SyncWebserver(ctx); err != nil {
		s.rollbackRow(ctx, site.ID, "webserver")
		return nil, fmt.Errorf("apply webserver config: %w", err)
	}
	return site, nil
}

// rollbackRow undoes the database half of a failed create. The files are left
// in place deliberately: they are harmless, and deleting a directory tree
// after a partial failure risks removing data the next attempt would reuse.
func (s *Service) rollbackRow(ctx context.Context, id int64, stage string) {
	if err := s.db.DeleteSite(ctx, id); err != nil {
		s.log.Error("sites: could not roll back site row after failed "+stage,
			"site_id", id, "err", err)
	}
}

// Delete removes a site. Files are removed only when asked.
func (s *Service) Delete(ctx context.Context, id int64, removeFiles bool) error {
	site, err := s.db.SiteByID(ctx, id)
	if err != nil {
		return err
	}
	if err := s.db.DeleteSite(ctx, id); err != nil {
		return err
	}
	// The configuration goes before the files: a site whose files are gone
	// but whose vhost still points at them serves confusing errors. If the
	// apply fails, put the row back rather than leaving a site that the
	// database has forgotten but the webserver is still serving.
	if err := s.SyncWebserver(ctx); err != nil {
		if _, rerr := s.db.CreateSite(ctx, site); rerr != nil {
			s.log.Error("sites: could not restore site row after a failed apply",
				"domain", site.Domain, "err", rerr)
		}
		return fmt.Errorf("apply webserver config: %w", err)
	}
	if removeFiles {
		if _, err := agentclient.Call[struct{}](ctx, s.agent, "site.remove", 1,
			actions.SiteRemoveRequest{
				Domain:    site.Domain,
				Owner:     site.OwnerUsername,
				VhostRoot: path.Dir(site.DocumentRoot),
			}); err != nil {
			return fmt.Errorf("remove files: %w", err)
		}
	}
	return nil
}

// UpdateRequest carries the fields a caller may change. A nil pointer means
// "leave alone", which keeps a partial update from clearing a field.
type UpdateRequest struct {
	AppType     *string
	PHPVersion  *string
	Aliases     *[]string
	RewriteMode *string
	Suspended   *bool
	WAFEnabled  *bool
}

// Update changes a site and re-applies the configuration.
func (s *Service) Update(ctx context.Context, id int64, req UpdateRequest) (*db.Site, error) {
	site, err := s.db.SiteByID(ctx, id)
	if err != nil {
		return nil, err
	}

	if req.AppType != nil {
		if !slices.Contains(webserver.AppTypes, *req.AppType) {
			return nil, fmt.Errorf("app type %q is not one of %v", *req.AppType, webserver.AppTypes)
		}
		site.AppType = *req.AppType
	}
	if req.PHPVersion != nil {
		site.PHPVersion = *req.PHPVersion
	}
	// Switching to static clears the version; switching away from it needs one.
	if site.AppType == webserver.AppStatic {
		site.PHPVersion = ""
	} else if !phpmgr.ValidVersion(site.PHPVersion) {
		return nil, fmt.Errorf("a %s site needs a PHP version", site.AppType)
	}
	if site.PHPVersion != "" {
		if err := s.requirePHP(ctx, site.PHPVersion); err != nil {
			return nil, err
		}
	}
	if req.Aliases != nil {
		aliases := make([]string, 0, len(*req.Aliases))
		for _, a := range *req.Aliases {
			a = strings.ToLower(strings.TrimSpace(a))
			if a == "" || a == site.Domain {
				continue
			}
			if !webserver.ValidDomain(a) {
				return nil, fmt.Errorf("alias %q is not a valid hostname", a)
			}
			aliases = append(aliases, a)
		}
		aliases = slices.Compact(slices.Sorted(slices.Values(aliases)))
		if err := s.checkHostnamesFree(ctx, site.Domain, aliases, site.ID); err != nil {
			return nil, err
		}
		site.Aliases = aliases
	}
	if req.RewriteMode != nil {
		if !slices.Contains(webserver.RewriteModes, *req.RewriteMode) {
			return nil, fmt.Errorf("rewrite mode %q is not one of %v", *req.RewriteMode, webserver.RewriteModes)
		}
		site.RewriteMode = *req.RewriteMode
	}
	if req.Suspended != nil {
		site.Suspended = *req.Suspended
	}
	if req.WAFEnabled != nil {
		site.WAFEnabled = *req.WAFEnabled
	}

	// Keep the row as it was, so a failed apply can put it back. Without
	// this the database would claim a change that the served configuration
	// never received -- the two would stay apart until some later, unrelated
	// sync happened to push it through.
	previous, err := s.db.SiteByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.db.UpdateSite(ctx, site); err != nil {
		return nil, err
	}
	if err := s.SyncWebserver(ctx); err != nil {
		if rerr := s.db.UpdateSite(ctx, previous); rerr != nil {
			s.log.Error("sites: could not restore site row after a failed apply",
				"site_id", id, "err", rerr)
		}
		return nil, fmt.Errorf("apply webserver config: %w", err)
	}
	return s.db.SiteByID(ctx, id)
}

// CertificateRequest asks for a certificate covering a site.
type CertificateRequest struct {
	SiteID int64
	Email  string
	// IncludeAliases puts the site's aliases on the certificate too. Off by
	// default: one alias whose DNS does not point here fails the whole order,
	// and losing the primary name to a stale alias is the worse outcome.
	IncludeAliases bool
	Staging        bool
	ForceHTTPS     bool
}

// IssueCertificate obtains a certificate for a site and turns TLS on.
func (s *Service) IssueCertificate(ctx context.Context, req CertificateRequest) (*db.Site, error) {
	site, err := s.db.SiteByID(ctx, req.SiteID)
	if err != nil {
		return nil, err
	}
	domains := []string{site.Domain}
	if req.IncludeAliases {
		domains = append(domains, site.Aliases...)
	}
	// Almost every customer wants www on the certificate, and almost none
	// think to ask. It is added when DNS already sends www to the same place
	// as the bare name, and left out otherwise -- an unresolvable name on the
	// order fails the whole thing, and losing the certificate the customer
	// actually asked for is much worse than not having www on it.
	if www := wwwSibling(site.Domain); www != "" && !slices.Contains(domains, www) {
		if sameDestination(ctx, site.Domain, www) {
			domains = append(domains, www)
			s.log.Info("sites: including www on the certificate", "domain", site.Domain)
		} else {
			s.log.Info("sites: leaving www off the certificate, DNS does not point here",
				"domain", site.Domain)
		}
	}

	cert, err := agentclient.Call[acme.Certificate](ctx, s.agent, "cert.issue", 1,
		actions.CertIssueRequest{Domains: domains, Email: req.Email, Staging: req.Staging})
	if err != nil {
		return nil, err
	}

	site.CertFile = cert.CertFile
	site.KeyFile = cert.KeyFile
	site.CertExpires = cert.NotAfter
	site.SSLEnabled = true
	site.ForceHTTPS = req.ForceHTTPS
	if err := s.db.UpdateSite(ctx, site); err != nil {
		return nil, err
	}
	if err := s.SyncWebserver(ctx); err != nil {
		return nil, fmt.Errorf("apply webserver config: %w", err)
	}
	return s.db.SiteByID(ctx, req.SiteID)
}

// DisableTLS turns HTTPS off for a site, leaving the certificate on disk so
// turning it back on does not need a fresh order against the rate limit.
func (s *Service) DisableTLS(ctx context.Context, siteID int64) (*db.Site, error) {
	site, err := s.db.SiteByID(ctx, siteID)
	if err != nil {
		return nil, err
	}
	site.SSLEnabled = false
	site.ForceHTTPS = false
	if err := s.db.UpdateSite(ctx, site); err != nil {
		return nil, err
	}
	if err := s.SyncWebserver(ctx); err != nil {
		return nil, fmt.Errorf("apply webserver config: %w", err)
	}
	return s.db.SiteByID(ctx, siteID)
}

// RenewDueCertificates re-issues certificates close to expiry.
//
// Runs on a timer rather than on demand: a certificate that lapses takes the
// site down for every visitor, and nobody notices a renewal that quietly
// worked. One failure does not stop the sweep — the next site may well
// succeed, and a single misconfigured domain should not hold up the rest.
func (s *Service) RenewDueCertificates(ctx context.Context, email string) (renewed int, errs []error) {
	due, err := s.db.SitesNeedingRenewal(ctx, time.Now().Add(acme.RenewBefore))
	if err != nil {
		return 0, []error{err}
	}
	for _, site := range due {
		if _, err := s.IssueCertificate(ctx, CertificateRequest{
			SiteID:     site.ID,
			Email:      email,
			ForceHTTPS: site.ForceHTTPS,
		}); err != nil {
			s.log.Error("sites: certificate renewal failed",
				"domain", site.Domain, "expires", site.CertExpires, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", site.Domain, err))
			continue
		}
		s.log.Info("sites: certificate renewed", "domain", site.Domain)
		renewed++
	}
	return renewed, errs
}

// wwwSibling returns the www form of a hostname, or "" when there is not one
// worth asking about: a name that is already www, or one deep enough that
// www in front of it would be unusual.
func wwwSibling(domain string) string {
	if strings.HasPrefix(domain, "www.") {
		return ""
	}
	if strings.Count(domain, ".") > 2 {
		return ""
	}
	return "www." + domain
}

// sameDestination reports whether two names resolve to at least one address
// in common.
//
// Comparing the two names against each other rather than against the
// server's own addresses is deliberate: behind a CDN or a load balancer the
// site does not resolve to this machine at all, and the question that
// matters is whether www lands wherever the bare name lands.
func sameDestination(ctx context.Context, a, b string) bool {
	lookup := func(host string) map[string]bool {
		ips, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil {
			return nil
		}
		out := make(map[string]bool, len(ips))
		for _, ip := range ips {
			out[ip] = true
		}
		return out
	}
	first := lookup(a)
	if len(first) == 0 {
		return false
	}
	for ip := range lookup(b) {
		if first[ip] {
			return true
		}
	}
	return false
}

// ChangeOwner hands a website, and its files, to another account.
//
// The files move with it. A site is defined by the directory it is served
// from, and that directory lives in its owner's home -- leaving the files
// behind would give the new owner a site they cannot reach over SFTP and
// leave the old owner paying quota for a site that is no longer theirs.
func (s *Service) ChangeOwner(ctx context.Context, siteID, newOwnerID int64) (*db.Site, error) {
	site, err := s.db.SiteByID(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if site.OwnerID == newOwnerID {
		return site, nil
	}
	newOwner, err := s.db.UserByID(ctx, newOwnerID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrOwnerNotFound
	}
	if err != nil {
		return nil, err
	}
	if !linuxuser.ValidName(newOwner.Username) {
		return nil, fmt.Errorf(
			"%w: %q is a staff account with no home directory to move the site into",
			ErrOwnerNotEligible, newOwner.Username)
	}
	// The receiving account has to be within its own package, or moving a
	// site would be a way around the limit that creating one enforces.
	if s.limits != nil {
		if err := s.limits.CheckSite(ctx, newOwner.ID); err != nil {
			return nil, err
		}
	}
	if err := s.EnsureAccount(ctx, newOwner); err != nil {
		return nil, err
	}

	oldOwner := site.OwnerUsername
	res, err := agentclient.Call[actions.SiteMoveResult](ctx, s.agent, "site.move", 1,
		actions.SiteMoveRequest{Domain: site.Domain, From: oldOwner, To: newOwner.Username})
	if err != nil {
		return nil, err
	}

	site.OwnerID = newOwner.ID
	site.DocumentRoot = res.DocumentRoot
	if err := s.db.UpdateSiteOwner(ctx, site.ID, newOwner.ID, res.DocumentRoot); err != nil {
		// The files are already in the new home. Saying so is the useful
		// thing: the operator can fix the row, and moving them back
		// automatically could fail the same way and leave nothing certain.
		return nil, fmt.Errorf(
			"the files were moved to %s but the panel could not record the new owner: %w",
			res.VhostRoot, err)
	}
	if err := s.SyncWebserver(ctx); err != nil {
		return nil, fmt.Errorf("apply webserver config: %w", err)
	}
	s.log.Info("sites: owner changed",
		"domain", site.Domain, "from", oldOwner, "to", newOwner.Username, "files", res.Files)
	return s.db.SiteByID(ctx, siteID)
}

// AttachCertificate points a site at a certificate that already exists on
// disk, whoever obtained it.
//
// Shared by the wildcard, reuse and manual paths: all three end with "this
// site now serves these two files", and having one place that writes the
// row and re-renders the webserver means the three cannot drift.
func (s *Service) AttachCertificate(ctx context.Context, siteID int64,
	certFile, keyFile string, notAfter time.Time, forceHTTPS bool) (*db.Site, error) {
	site, err := s.db.SiteByID(ctx, siteID)
	if err != nil {
		return nil, err
	}
	site.CertFile = certFile
	site.KeyFile = keyFile
	site.CertExpires = notAfter
	site.SSLEnabled = true
	site.ForceHTTPS = forceHTTPS
	if err := s.db.UpdateSite(ctx, site); err != nil {
		return nil, err
	}
	if err := s.SyncWebserver(ctx); err != nil {
		return nil, fmt.Errorf("apply webserver config: %w", err)
	}
	return s.db.SiteByID(ctx, siteID)
}

// SyncWebserver regenerates the whole webserver configuration from the
// database. Every mutation ends here, which is what guarantees the rendered
// configuration and the database never drift apart.
func (s *Service) SyncWebserver(ctx context.Context) error {
	rows, err := s.db.ListSites(ctx, db.ScopeAll())
	if err != nil {
		return err
	}
	// One query for every site's overrides rather than one per site: this
	// runs on every change to any site.
	phpSettings, err := s.db.AllSitePHPSettings(ctx)
	if err != nil {
		return err
	}
	specs := make([]actions.SiteSpec, 0, len(rows))
	for _, r := range rows {
		specs = append(specs, actions.SiteSpec{
			Domain:       r.Domain,
			Aliases:      r.Aliases,
			Owner:        r.OwnerUsername,
			VhostRoot:    path.Dir(r.DocumentRoot),
			DocumentRoot: r.DocumentRoot,
			AppType:      r.AppType,
			PHPVersion:   r.PHPVersion,
			RewriteMode:  r.RewriteMode,
			SSLEnabled:   r.SSLEnabled,
			CertFile:     r.CertFile,
			KeyFile:      r.KeyFile,
			ForceHTTPS:   r.ForceHTTPS,
			WAFEnabled:   r.WAFEnabled,
			Suspended:    r.Suspended,
			PHPSettings:  phpSettings[r.ID],
		})
	}
	_, err = agentclient.Call[struct{}](ctx, s.agent, "webserver.apply", 1,
		actions.WebserverApplyRequest{Config: s.cfg, Sites: specs})
	return err
}

// EnsureAccount makes sure a panel user has its Linux account. It is safe to
// call repeatedly; the agent action is itself idempotent.
func (s *Service) EnsureAccount(ctx context.Context, u *db.User) error {
	if u.LinuxUID != nil && *u.LinuxUID > 0 {
		return nil
	}
	acct, err := agentclient.Call[linuxuser.Account](ctx, s.agent, "linuxuser.create", 1,
		actions.AccountRequest{Username: u.Username})
	if err != nil {
		return fmt.Errorf("create Linux account for %q: %w", u.Username, err)
	}
	return s.db.SetUserLinuxAccount(ctx, u.ID, acct.UID, acct.Home)
}

// SetSFTPPassword sets the owner's SFTP password.
func (s *Service) SetSFTPPassword(ctx context.Context, u *db.User, password string) error {
	if err := s.EnsureAccount(ctx, u); err != nil {
		return err
	}
	_, err := agentclient.Call[struct{}](ctx, s.agent, "linuxuser.set_password", 1,
		actions.AccountPasswordRequest{Username: u.Username, Password: password})
	return err
}

// checkHostnamesFree rejects a hostname already claimed by another site. The
// renderer would catch this too, but failing here names the request that is
// wrong instead of failing every later apply.
func (s *Service) checkHostnamesFree(ctx context.Context, domain string, aliases []string, exceptID int64) error {
	rows, err := s.db.ListSites(ctx, db.ScopeAll())
	if err != nil {
		return err
	}
	taken := make(map[string]string)
	for _, r := range rows {
		if r.ID == exceptID {
			continue
		}
		for _, h := range append([]string{r.Domain}, r.Aliases...) {
			taken[h] = r.Domain
		}
	}
	for _, h := range append([]string{domain}, aliases...) {
		if owner, dup := taken[h]; dup {
			return fmt.Errorf("%w: %q is already served by %q", ErrDomainTaken, h, owner)
		}
	}
	return nil
}

// requirePHP fails when the version is not installed, rather than letting the
// webserver start an interpreter that is not there and return 503 to visitors.
func (s *Service) requirePHP(ctx context.Context, version string) error {
	res, err := agentclient.Call[actions.PHPListResult](ctx, s.agent, "php.list", 1, struct{}{})
	if err != nil {
		return err
	}
	for _, v := range res.Versions {
		if v.Version == version {
			if v.Installed {
				return nil
			}
			return fmt.Errorf("%w: PHP %s (install it first)", ErrPHPNotReady, version)
		}
	}
	return fmt.Errorf("%w: PHP %s is not offered by provider %q", ErrPHPNotReady, version, res.Provider)
}

// VisibleTo is gone. Listings take an auth.ScopeFor(user) now, which can
// express "mine and my customers'" -- the case a reseller needs and a single
// owner id cannot describe.
