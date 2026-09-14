// Package panelusers manages panel accounts.
//
// A panel user of role end_user also gets a Linux account, because their
// sites' PHP runs as it and they reach their files over SFTP as it. Creating
// and deleting a panel user therefore reaches the host, which is why this
// lives in a service rather than in the handlers.
package panelusers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
)

// Service manages panel users.
type Service struct {
	db    *db.DB
	agent *agentclient.Client
	log   *slog.Logger
}

// New builds a Service.
func New(database *db.DB, ac *agentclient.Client, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: database, agent: ac, log: log}
}

// Errors callers branch on.
var (
	ErrUsernameTaken = errors.New("panelusers: username already exists")
	ErrBadUsername   = errors.New("panelusers: invalid username")
	ErrLastAdmin     = errors.New("panelusers: this is the last administrator")
	ErrOwnsResources = errors.New("panelusers: account still owns sites or databases")
)

// CreateRequest describes a new panel user.
type CreateRequest struct {
	Username string
	Email    string
	Role     auth.Role
	// Password is optional. When empty one is generated and returned.
	Password string
	// ParentID is the reseller the account belongs to, zero for an account
	// that belongs to the server itself.
	ParentID int64
}

// Result carries the created user plus any generated secret, which is shown
// once and never stored in a form the panel can read back.
type Result struct {
	User     *db.User
	Password string
	// SFTPPassword is set for end users, whose Linux account is what they
	// use to upload files.
	SFTPPassword string
}

// Create provisions a panel user and, for an end user, the Linux account
// behind it.
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Result, error) {
	username := strings.ToLower(strings.TrimSpace(req.Username))
	if !req.Role.Valid() {
		return nil, fmt.Errorf("role %q is not valid", req.Role)
	}
	// An end user's name becomes a Linux account, so it has to satisfy the
	// stricter of the two rule sets from the start rather than failing later
	// when their first site is created.
	if req.Role == auth.RoleEndUser {
		if !linuxuser.ValidName(username) {
			return nil, fmt.Errorf("%w: %q cannot be a Linux account name — 3 to 32 characters, "+
				"starting with a letter, lowercase letters, digits, _ and - only, and not a reserved name",
				ErrBadUsername, username)
		}
	} else if !staffNamePattern(username) {
		return nil, fmt.Errorf("%w: %q — 3 to 32 characters, letters, digits, _ . and - only",
			ErrBadUsername, username)
	}

	if _, err := s.db.UserByUsername(ctx, username); err == nil {
		return nil, fmt.Errorf("%w: %q", ErrUsernameTaken, username)
	} else if !errors.Is(err, db.ErrNotFound) {
		return nil, err
	}

	password := req.Password
	generated := false
	if password == "" {
		codes, err := auth.NewRecoveryCodes(1)
		if err != nil {
			return nil, err
		}
		password = codes[0]
		generated = true
	}
	if len(password) < 10 {
		return nil, errors.New("panelusers: password must be at least 10 characters")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}

	newUser := &db.User{
		Username:     username,
		Email:        strings.TrimSpace(req.Email),
		PasswordHash: hash,
		Role:         string(req.Role),
	}
	if req.ParentID != 0 {
		newUser.ParentID = &req.ParentID
	}
	user, err := s.db.CreateUser(ctx, newUser)
	if err != nil {
		return nil, err
	}

	res := &Result{User: user}
	if generated {
		res.Password = password
	}

	if req.Role == auth.RoleEndUser {
		acct, err := agentclient.Call[linuxuser.Account](ctx, s.agent, "linuxuser.create", 1,
			actions.AccountRequest{Username: username})
		if err != nil {
			// Roll the panel record back: an end user with no Linux account
			// cannot own a site, so leaving the row would hand the operator a
			// user that fails at the next step for no visible reason.
			if derr := s.db.DeleteUser(ctx, user.ID); derr != nil {
				s.log.Error("panelusers: could not roll back after failed account creation",
					"user", username, "err", derr)
			}
			return nil, fmt.Errorf("create Linux account: %w", err)
		}
		if err := s.db.SetUserLinuxAccount(ctx, user.ID, acct.UID, acct.Home); err != nil {
			return nil, err
		}

		sftp, err := auth.NewRecoveryCodes(1)
		if err != nil {
			return nil, err
		}
		if _, err := agentclient.Call[struct{}](ctx, s.agent, "linuxuser.set_password", 1,
			actions.AccountPasswordRequest{Username: username, Password: sftp[0]}); err != nil {
			return nil, fmt.Errorf("set SFTP password: %w", err)
		}
		res.SFTPPassword = sftp[0]
		res.User, _ = s.db.UserByID(ctx, user.ID)
	}
	return res, nil
}

// staffNamePattern allows a slightly wider name for accounts that never get a
// Linux account behind them.
func staffNamePattern(s string) bool {
	if len(s) < 3 || len(s) > 32 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '_' || r == '.' || r == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// Delete removes a panel user, and the Linux account when there is one.
//
// Refused while the account still owns anything. Cascading would destroy a
// customer's sites and databases on a single click, and the database's
// RESTRICT constraints would refuse it anyway — better to say why.
func (s *Service) Delete(ctx context.Context, id int64, removeFiles bool) error {
	user, err := s.db.UserByID(ctx, id)
	if err != nil {
		return err
	}
	sites, dbs, err := s.db.UserResourceCounts(ctx, id)
	if err != nil {
		return err
	}
	if sites > 0 || dbs > 0 {
		return fmt.Errorf("%w: %d site(s) and %d database(s) must be removed first",
			ErrOwnsResources, sites, dbs)
	}
	if auth.Role(user.Role) == auth.RoleAdmin {
		others, err := s.db.CountAdmins(ctx, id)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}

	if err := s.db.DeleteUser(ctx, id); err != nil {
		return err
	}
	if user.LinuxUID != nil && *user.LinuxUID > 0 {
		if _, err := agentclient.Call[struct{}](ctx, s.agent, "linuxuser.delete", 1,
			actions.AccountDeleteRequest{Username: user.Username, RemoveHome: removeFiles}); err != nil {
			// The panel record is already gone; report it but do not fail the
			// whole operation, or the operator is left unable to retry.
			s.log.Error("panelusers: panel record removed but the Linux account remains",
				"user", user.Username, "err", err)
			return fmt.Errorf("panel account removed, but the Linux account could not be deleted: %w", err)
		}
	}
	return nil
}

// SetSuspended suspends or restores an account. Suspending revokes every live
// session immediately rather than at next login.
func (s *Service) SetSuspended(ctx context.Context, id int64, suspended bool) error {
	user, err := s.db.UserByID(ctx, id)
	if err != nil {
		return err
	}
	if suspended && auth.Role(user.Role) == auth.RoleAdmin {
		others, err := s.db.CountAdmins(ctx, id)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	if err := s.db.SetUserSuspended(ctx, id, suspended); err != nil {
		return err
	}
	if suspended {
		return s.db.DeleteUserSessions(ctx, id)
	}
	return nil
}

// UpdateProfile changes email and role.
func (s *Service) UpdateProfile(ctx context.Context, id int64, email string, role auth.Role) error {
	user, err := s.db.UserByID(ctx, id)
	if err != nil {
		return err
	}
	if !role.Valid() {
		return fmt.Errorf("role %q is not valid", role)
	}
	// Demoting the last administrator would lock everyone out of the panel.
	if auth.Role(user.Role) == auth.RoleAdmin && role != auth.RoleAdmin {
		others, err := s.db.CountAdmins(ctx, id)
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}
	return s.db.UpdateUserProfile(ctx, id, strings.TrimSpace(email), string(role))
}

// SetPassword replaces a user's panel password and ends their sessions, so a
// reset actually removes whoever was signed in.
func (s *Service) SetPassword(ctx context.Context, id int64, password string) (string, error) {
	generated := false
	if password == "" {
		codes, err := auth.NewRecoveryCodes(1)
		if err != nil {
			return "", err
		}
		password = codes[0]
		generated = true
	}
	if len(password) < 10 {
		return "", errors.New("panelusers: password must be at least 10 characters")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", err
	}
	if err := s.db.SetPasswordHash(ctx, id, hash); err != nil {
		return "", err
	}
	if err := s.db.DeleteUserSessions(ctx, id); err != nil {
		return "", err
	}
	if generated {
		return password, nil
	}
	return "", nil
}

// SetSFTPPassword sets the Linux account password an end user uploads with.
func (s *Service) SetSFTPPassword(ctx context.Context, id int64) (string, error) {
	user, err := s.db.UserByID(ctx, id)
	if err != nil {
		return "", err
	}
	if user.LinuxUID == nil || *user.LinuxUID == 0 {
		return "", errors.New("panelusers: this account has no Linux user")
	}
	codes, err := auth.NewRecoveryCodes(1)
	if err != nil {
		return "", err
	}
	if _, err := agentclient.Call[struct{}](ctx, s.agent, "linuxuser.set_password", 1,
		actions.AccountPasswordRequest{Username: user.Username, Password: codes[0]}); err != nil {
		return "", err
	}
	return codes[0], nil
}
