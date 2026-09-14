// Package databases manages MariaDB databases and accounts for panel users.
//
// Names are always prefixed with the owner's username, the way cPanel and
// DirectAdmin do it. Three reasons: two customers can both want a database
// called "wordpress"; a name tells you at a glance who owns it; and Stage B's
// CloudLinux MySQL Governor maps database accounts to system users using
// exactly this convention.
package databases

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/dbms"
)

// Limits is the package check a database creation must pass.
type Limits interface {
	CheckDatabase(ctx context.Context, ownerID int64) error
}

// Service creates and removes databases through the agent.
type Service struct {
	db     *db.DB
	agent  *agentclient.Client
	log    *slog.Logger
	limits Limits
}

// New builds a Service.
func New(database *db.DB, ac *agentclient.Client, limits Limits, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: database, agent: ac, log: log, limits: limits}
}

// Errors callers branch on.
var (
	ErrNameTaken     = errors.New("databases: name already in use")
	ErrOwnerNotFound = errors.New("databases: owner does not exist")
	ErrBadSuffix     = errors.New("databases: invalid name")
)

// suffixPattern is what a user may type. The prefix is added by the panel, so
// the input is only the part after it.
var suffixPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ValidSuffix reports whether s is an acceptable user-supplied name part.
func ValidSuffix(s string) bool { return suffixPattern.MatchString(s) }

// GeneratePassword returns a strong password for a database account.
func GeneratePassword() (string, error) {
	b := make([]byte, 18) // 24 base64url characters
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("databases: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// resolveOwner loads the owner and checks the resulting name fits MariaDB's
// length limits, which is easy to exceed once a prefix is added.
func (s *Service) resolveOwner(ctx context.Context, ownerID int64, suffix string, maxLen int) (*db.User, string, error) {
	if !ValidSuffix(suffix) {
		return nil, "", fmt.Errorf("%w: %q — lowercase letters, digits and underscore only, starting with a letter", ErrBadSuffix, suffix)
	}
	owner, err := s.db.UserByID(ctx, ownerID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, "", ErrOwnerNotFound
	}
	if err != nil {
		return nil, "", err
	}
	full := dbms.Prefixed(owner.Username, suffix)
	if len(full) > maxLen {
		return nil, "", fmt.Errorf("%w: %q is %d characters with the %q prefix, limit is %d",
			ErrBadSuffix, full, len(full), owner.Username+"_", maxLen)
	}
	return owner, full, nil
}

// CreateDatabase creates a database owned by ownerID.
//
// The server comes first and the record second: a database that exists on the
// server but not in the panel is visible and fixable, while a record with no
// database behind it hands the user a connection string that fails.
func (s *Service) CreateDatabase(ctx context.Context, ownerID int64, suffix string) (*db.Database, error) {
	_, name, err := s.resolveOwner(ctx, ownerID, suffix, dbms.MaxDatabaseName)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.DatabaseByName(ctx, name); err == nil {
		return nil, fmt.Errorf("%w: database %q", ErrNameTaken, name)
	} else if !errors.Is(err, db.ErrNotFound) {
		return nil, err
	}
	if s.limits != nil {
		if err := s.limits.CheckDatabase(ctx, ownerID); err != nil {
			return nil, err
		}
	}

	if _, err := agentclient.Call[struct{}](ctx, s.agent, "db.create", 1,
		actions.DatabaseRequest{Name: name}); err != nil {
		return nil, err
	}
	rec, err := s.db.CreateDatabaseRecord(ctx, name, ownerID)
	if err != nil {
		// Undo the server-side half so a retry is not blocked by a database
		// the panel does not know about.
		if _, derr := agentclient.Call[struct{}](ctx, s.agent, "db.drop", 1,
			actions.DatabaseRequest{Name: name}); derr != nil {
			s.log.Error("databases: created on the server but not recorded, and could not be dropped",
				"database", name, "err", derr)
		}
		return nil, err
	}
	return rec, nil
}

// DeleteDatabase drops a database and forgets it.
func (s *Service) DeleteDatabase(ctx context.Context, id int64) error {
	rec, err := s.db.DatabaseByID(ctx, id)
	if err != nil {
		return err
	}
	if _, err := agentclient.Call[struct{}](ctx, s.agent, "db.drop", 1,
		actions.DatabaseRequest{Name: rec.Name}); err != nil {
		return err
	}
	return s.db.DeleteDatabaseRecord(ctx, id)
}

// CreateUser creates a database account and returns its generated password,
// which is shown once and never stored.
func (s *Service) CreateUser(ctx context.Context, ownerID int64, suffix string) (*db.DBUser, string, error) {
	_, username, err := s.resolveOwner(ctx, ownerID, suffix, dbms.MaxUserName)
	if err != nil {
		return nil, "", err
	}
	if _, err := s.db.DBUserByName(ctx, username); err == nil {
		return nil, "", fmt.Errorf("%w: user %q", ErrNameTaken, username)
	} else if !errors.Is(err, db.ErrNotFound) {
		return nil, "", err
	}

	password, err := GeneratePassword()
	if err != nil {
		return nil, "", err
	}
	if _, err := agentclient.Call[struct{}](ctx, s.agent, "dbuser.create", 1,
		actions.DBUserPasswordRequest{Username: username, Password: password}); err != nil {
		return nil, "", err
	}
	rec, err := s.db.CreateDBUserRecord(ctx, username, ownerID)
	if err != nil {
		if _, derr := agentclient.Call[struct{}](ctx, s.agent, "dbuser.drop", 1,
			actions.DBUserRequest{Username: username}); derr != nil {
			s.log.Error("databases: account created on the server but not recorded, and could not be dropped",
				"user", username, "err", derr)
		}
		return nil, "", err
	}
	return rec, password, nil
}

// DeleteUser drops an account and forgets it.
func (s *Service) DeleteUser(ctx context.Context, id int64) error {
	rec, err := s.db.DBUserByID(ctx, id)
	if err != nil {
		return err
	}
	if _, err := agentclient.Call[struct{}](ctx, s.agent, "dbuser.drop", 1,
		actions.DBUserRequest{Username: rec.Username}); err != nil {
		return err
	}
	return s.db.DeleteDBUserRecord(ctx, id)
}

// ResetPassword sets a new generated password and returns it.
func (s *Service) ResetPassword(ctx context.Context, id int64) (string, error) {
	rec, err := s.db.DBUserByID(ctx, id)
	if err != nil {
		return "", err
	}
	password, err := GeneratePassword()
	if err != nil {
		return "", err
	}
	if _, err := agentclient.Call[struct{}](ctx, s.agent, "dbuser.password", 1,
		actions.DBUserPasswordRequest{Username: rec.Username, Password: password}); err != nil {
		return "", err
	}
	return password, nil
}

// Grant gives an account access to a database. Both must have the same owner:
// without that check an administrator could wire one customer's account to
// another customer's data with a single mistyped id.
func (s *Service) Grant(ctx context.Context, databaseID, userID int64) error {
	database, dbUser, err := s.pair(ctx, databaseID, userID)
	if err != nil {
		return err
	}
	if _, err := agentclient.Call[struct{}](ctx, s.agent, "db.grant", 1,
		actions.GrantRequest{Username: dbUser.Username, Database: database.Name}); err != nil {
		return err
	}
	return s.db.AddGrant(ctx, databaseID, userID)
}

// Revoke removes an account's access to a database.
func (s *Service) Revoke(ctx context.Context, databaseID, userID int64) error {
	database, dbUser, err := s.pair(ctx, databaseID, userID)
	if err != nil {
		return err
	}
	if _, err := agentclient.Call[struct{}](ctx, s.agent, "db.revoke", 1,
		actions.GrantRequest{Username: dbUser.Username, Database: database.Name}); err != nil {
		return err
	}
	return s.db.RemoveGrant(ctx, databaseID, userID)
}

func (s *Service) pair(ctx context.Context, databaseID, userID int64) (*db.Database, *db.DBUser, error) {
	database, err := s.db.DatabaseByID(ctx, databaseID)
	if err != nil {
		return nil, nil, err
	}
	dbUser, err := s.db.DBUserByID(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	if database.OwnerID != dbUser.OwnerID {
		return nil, nil, fmt.Errorf("database %q and user %q belong to different owners",
			database.Name, dbUser.Username)
	}
	return database, dbUser, nil
}

// ListDatabases returns databases with their sizes filled in.
func (s *Service) ListDatabases(ctx context.Context, scope db.Scope) ([]*db.Database, error) {
	rows, err := s.db.ListDatabases(ctx, scope)
	if err != nil {
		return nil, err
	}
	// Size comes from the server, one call each. A failure is not fatal:
	// showing a list without sizes beats showing an error page.
	for _, r := range rows {
		res, err := agentclient.Call[actions.DatabaseSizeResult](ctx, s.agent, "db.size", 1,
			actions.DatabaseRequest{Name: r.Name})
		if err != nil {
			s.log.Warn("databases: could not read size", "database", r.Name, "err", err)
			continue
		}
		r.SizeBytes = res.SizeBytes
	}
	return rows, nil
}

// ConnectionHost is what a site's application should connect to. The socket
// is faster, but a hostname works from every language without configuration.
const ConnectionHost = "127.0.0.1"

// VisibleTo is gone. Listings take an auth.ScopeFor(user) now, which can
// express "mine and my customers'" -- the case a reseller needs and a single
// owner id cannot describe.

// SuffixOf strips the owner prefix for display, so a user sees the part they
// chose rather than the full name every time.
func SuffixOf(owner, full string) string {
	return strings.TrimPrefix(full, owner+"_")
}
