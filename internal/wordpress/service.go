// Package wordpress installs WordPress into a site the panel already owns.
//
// One click covers what is otherwise four separate jobs: a database, a
// database account with a grant on it, the WordPress files, and the install
// itself. Doing them together is the point -- a customer who has to do them
// in order, by hand, in the right order, is a customer who opens a ticket.
package wordpress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/databases"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/dbms"
)

// Service performs one-click installs.
type Service struct {
	db        *db.DB
	agent     *agentclient.Client
	slow      *agentclient.Client
	databases *databases.Service
	log       *slog.Logger
}

// New builds the service.
func New(database *db.DB, ac *agentclient.Client, dbSvc *databases.Service, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		db: database, agent: ac, slow: ac.WithTimeout(actions.WPBudget),
		databases: dbSvc, log: log,
	}
}

// Status reports what is in a site's document root.
func (s *Service) Status(ctx context.Context, site *db.Site) (actions.WPStatus, error) {
	return agentclient.Call[actions.WPStatus](ctx, s.agent, "wp.status", 1,
		actions.WPPathRequest{Owner: site.OwnerUsername, DocumentRoot: site.DocumentRoot})
}

// Request describes an install.
type Request struct {
	Site  *db.Site
	Title string
	// AdminUser and AdminEmail identify the WordPress administrator, which is
	// not the same thing as the panel account: one person may run several
	// WordPress sites from one hosting account.
	AdminUser  string
	AdminEmail string
	Locale     string
	// UseHTTPS decides the site URL WordPress records. It matters more than
	// it looks: WordPress writes the URL into the database and then refuses
	// to serve on any other, so getting it wrong means a site that redirects
	// to a scheme it cannot answer on.
	UseHTTPS bool
}

// Result carries what the customer needs to log in, once.
type Result struct {
	Version       string `json:"version"`
	SiteURL       string `json:"site_url"`
	AdminURL      string `json:"admin_url"`
	AdminUser     string `json:"admin_user"`
	AdminPassword string `json:"admin_password"`
	Database      string `json:"database"`
	DatabaseUser  string `json:"database_user"`
}

// CanReach reports whether the actor may install into this site.
func CanReach(actor *db.User, site *db.Site) bool {
	return auth.OwnsResource(actor, site.OwnerID, site.OwnerParentID)
}

// Install provisions a database, an account for it, and WordPress itself.
//
// The database is created first because it is the step that can fail for a
// reason the customer can act on -- a package limit -- and failing before any
// files are written leaves nothing to clean up.
func (s *Service) Install(ctx context.Context, req Request) (*Result, error) {
	site := req.Site
	if site.AppType != "wordpress" {
		return nil, fmt.Errorf("this site is set up as %q; change it to WordPress first", site.AppType)
	}

	st, err := s.Status(ctx, site)
	if err != nil {
		return nil, err
	}
	if st.Installed {
		return nil, errors.New("this site already has WordPress in it")
	}
	if !st.CLIReady {
		return nil, errors.New("WP-CLI is not installed on this server yet")
	}
	if !st.Empty {
		return nil, errors.New(
			"the document root is not empty; move what is there out of the way first")
	}

	suffix := dbSuffix(site.Domain)
	database, err := s.ensureDatabase(ctx, site.OwnerID, suffix)
	if err != nil {
		return nil, fmt.Errorf("create the database: %w", err)
	}
	dbUser, dbPassword, err := s.ensureUser(ctx, site.OwnerID, suffix)
	if err != nil {
		return nil, fmt.Errorf("create the database account: %w", err)
	}
	if err := s.databases.Grant(ctx, database.ID, dbUser.ID); err != nil {
		return nil, fmt.Errorf("grant the database account access: %w", err)
	}

	adminPassword, err := databases.GeneratePassword()
	if err != nil {
		return nil, err
	}

	scheme := "http"
	if req.UseHTTPS {
		scheme = "https"
	}
	siteURL := scheme + "://" + site.Domain

	res, err := agentclient.Call[actions.WPInstallResult](ctx, s.slow, "wp.install", 1,
		actions.WPInstallRequest{
			Owner: site.OwnerUsername, DocumentRoot: site.DocumentRoot,
			PHPVersion: site.PHPVersion, SiteURL: siteURL,
			DBName: database.Name, DBUser: dbUser.Username, DBPassword: dbPassword,
			Title: req.Title, AdminUser: req.AdminUser,
			AdminPassword: adminPassword, AdminEmail: req.AdminEmail,
			Locale: req.Locale,
		})
	if err != nil {
		// The database stays. Removing it would be tidier, but a failed
		// install often leaves files worth inspecting, and dropping the
		// database behind the operator's back is how evidence disappears.
		s.log.Error("wordpress: install failed",
			"domain", site.Domain, "database", database.Name, "err", err)
		return nil, err
	}

	s.log.Info("wordpress: installed", "domain", site.Domain, "version", res.Version)
	return &Result{
		Version: res.Version, SiteURL: siteURL,
		AdminURL:      strings.TrimSuffix(siteURL, "/") + "/wp-admin/",
		AdminUser:     req.AdminUser,
		AdminPassword: adminPassword,
		Database:      database.Name, DatabaseUser: dbUser.Username,
	}, nil
}

// ensureDatabase creates the site's database, or adopts the one a previous
// attempt left behind.
//
// An install that fails after the database exists -- a download that ran out
// of memory, a server that rebooted -- would otherwise be unretryable: the
// second attempt stops at "name already in use" and the customer is left to
// work out that they have to delete a database they never knowingly created.
// Adopting is safe because the name is derived from the domain and the check
// below confirms the owner.
func (s *Service) ensureDatabase(ctx context.Context, ownerID int64, suffix string) (*db.Database, error) {
	database, err := s.databases.CreateDatabase(ctx, ownerID, suffix)
	if err == nil {
		return database, nil
	}
	if !errors.Is(err, databases.ErrNameTaken) {
		return nil, err
	}

	owner, uerr := s.db.UserByID(ctx, ownerID)
	if uerr != nil {
		return nil, err
	}
	existing, ferr := s.db.DatabaseByName(ctx, dbms.Prefixed(owner.Username, suffix))
	if ferr != nil || existing.OwnerID != ownerID {
		return nil, err
	}
	s.log.Info("wordpress: reusing the database a previous attempt created",
		"database", existing.Name)
	return existing, nil
}

// ensureUser does the same for the database account. The password of an
// existing one cannot be read back, so it is reset: wp-config.php is about to
// be written with whatever this returns, and a password nobody knows is worse
// than a rotated one.
func (s *Service) ensureUser(ctx context.Context, ownerID int64, suffix string) (*db.DBUser, string, error) {
	dbUser, password, err := s.databases.CreateUser(ctx, ownerID, suffix)
	if err == nil {
		return dbUser, password, nil
	}
	if !errors.Is(err, databases.ErrNameTaken) {
		return nil, "", err
	}

	owner, uerr := s.db.UserByID(ctx, ownerID)
	if uerr != nil {
		return nil, "", err
	}
	existing, ferr := s.db.DBUserByName(ctx, dbms.Prefixed(owner.Username, suffix))
	if ferr != nil || existing.OwnerID != ownerID {
		return nil, "", err
	}
	password, rerr := s.databases.ResetPassword(ctx, existing.ID)
	if rerr != nil {
		return nil, "", rerr
	}
	s.log.Info("wordpress: reusing the database account a previous attempt created",
		"user", existing.Username)
	return existing, password, nil
}

// dbSuffix turns a domain into the short name a database gets after its
// owner prefix. "shop.example.com" becomes "shop", which is what a person
// looking at a list of databases can actually match to a site.
func dbSuffix(domain string) string {
	base := domain
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" || out[0] < 'a' || out[0] > 'z' {
		// A database name must start with a letter, and "123shop" or an
		// all-punctuation label would otherwise be rejected further down.
		out = "wp" + out
	}
	if len(out) > 16 {
		out = out[:16]
	}
	return out
}
