package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/db"
)

// Store is the persistence this package needs. Declaring it here rather than
// importing a concrete store keeps the dependency pointing one way and makes
// the service trivial to fake in tests.
type Store interface {
	UserByUsername(ctx context.Context, username string) (*db.User, error)
	UserByID(ctx context.Context, id int64) (*db.User, error)
	SetPasswordHash(ctx context.Context, userID int64, hash string) error

	CreateSession(ctx context.Context, s *db.Session) error
	SessionByID(ctx context.Context, id string) (*db.Session, error)
	TouchSession(ctx context.Context, id string, at time.Time) error
	DeleteSession(ctx context.Context, id string) error
	DeleteUserSessions(ctx context.Context, userID int64) error
	PurgeExpiredSessions(ctx context.Context, now time.Time) (int64, error)

	CreateAPIToken(ctx context.Context, t *db.APIToken) (*db.APIToken, error)
	APITokenByPrefix(ctx context.Context, prefix string) (*db.APIToken, error)
	TouchAPIToken(ctx context.Context, id int64, at time.Time) error

	RedeemRecoveryCode(ctx context.Context, userID int64, codeHash string) (bool, error)
}

// Authentication failures. Handlers must collapse ErrInvalidCredentials and
// any "no such user" case into one indistinguishable response.
var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrTOTPRequired       = errors.New("auth: two-factor code required")
	ErrTOTPInvalid        = errors.New("auth: invalid two-factor code")
	ErrSuspended          = errors.New("auth: account suspended")
	ErrSessionInvalid     = errors.New("auth: session invalid or expired")
	ErrTokenInvalid       = errors.New("auth: api token invalid")
)

// Service carries out authentication against a Store.
type Service struct {
	store      Store
	sessionTTL time.Duration
	idleTTL    time.Duration
	now        func() time.Time // swappable in tests
}

// NewService builds a Service. An idleTTL of 0 disables the idle timeout.
func NewService(store Store, sessionTTL, idleTTL time.Duration) *Service {
	return &Service{store: store, sessionTTL: sessionTTL, idleTTL: idleTTL, now: time.Now}
}

// dummyHash is verified against when no user matches, so a login attempt for
// an unknown username costs the same wall-clock time as one for a known
// username with a wrong password. Without this, response latency leaks which
// usernames exist.
var dummyHash string

func init() {
	h, err := HashPassword("this password matches nothing")
	if err != nil {
		panic("auth: cannot build dummy hash: " + err.Error())
	}
	dummyHash = h
}

// LoginResult is what a successful login yields.
type LoginResult struct {
	User    *db.User
	Session *db.Session
	// Cookie is the plaintext session value. It exists only in this struct
	// and in the Set-Cookie header; the store holds its hash.
	Cookie string
}

// Login verifies credentials and, when 2FA is on, a TOTP or recovery code.
//
// It returns ErrTOTPRequired when the password is correct but a second factor
// is still needed. Disclosing that is safe: the caller has already proven the
// password.
func (s *Service) Login(ctx context.Context, username, password, code, ip, userAgent string) (*LoginResult, error) {
	u, err := s.store.UserByUsername(ctx, strings.TrimSpace(username))
	if errors.Is(err, db.ErrNotFound) {
		_ = VerifyPassword(dummyHash, password)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}

	if err := VerifyPassword(u.PasswordHash, password); err != nil {
		if errors.Is(err, ErrMismatch) || errors.Is(err, ErrBadHash) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	// Checked after the password so a suspended account is not disclosed to
	// someone who does not already know the password.
	if u.Suspended {
		return nil, ErrSuspended
	}

	if u.TOTPEnabled {
		if strings.TrimSpace(code) == "" {
			return nil, ErrTOTPRequired
		}
		if err := s.checkSecondFactor(ctx, u, code); err != nil {
			return nil, err
		}
	}

	// Opportunistically upgrade hashes made with weaker parameters.
	if NeedsRehash(u.PasswordHash) {
		if nh, herr := HashPassword(password); herr == nil {
			_ = s.store.SetPasswordHash(ctx, u.ID, nh)
		}
	}

	sess, cookie, err := s.newSession(ctx, u.ID, ip, userAgent)
	if err != nil {
		return nil, err
	}
	return &LoginResult{User: u, Session: sess, Cookie: cookie}, nil
}

func (s *Service) checkSecondFactor(ctx context.Context, u *db.User, code string) error {
	if VerifyTOTP(u.TOTPSecret, code) {
		return nil
	}
	ok, err := s.store.RedeemRecoveryCode(ctx, u.ID, HashRecoveryCode(code))
	if err != nil {
		return err
	}
	if !ok {
		return ErrTOTPInvalid
	}
	return nil
}

func (s *Service) newSession(ctx context.Context, userID int64, ip, userAgent string) (*db.Session, string, error) {
	return s.newSessionAs(ctx, userID, 0, ip, userAgent)
}

// newSessionAs creates a session, optionally recording the member of staff
// driving it.
func (s *Service) newSessionAs(ctx context.Context, userID, impersonatorID int64, ip, userAgent string) (*db.Session, string, error) {
	cookie, err := randomToken(32)
	if err != nil {
		return nil, "", err
	}
	now := s.now().UTC()
	sess := &db.Session{
		ID:             hashSecret(cookie),
		UserID:         userID,
		ImpersonatorID: impersonatorID,
		CreatedAt:      now,
		ExpiresAt:      now.Add(s.sessionTTL),
		LastSeenAt:     now,
		IP:             ip,
		UserAgent:      truncate(userAgent, 512),
	}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return nil, "", err
	}
	return sess, cookie, nil
}

// ValidateSession resolves a cookie value to its user, enforcing both the
// absolute and the idle timeout.
func (s *Service) ValidateSession(ctx context.Context, cookie string) (*db.User, *db.Session, error) {
	if cookie == "" {
		return nil, nil, ErrSessionInvalid
	}
	sess, err := s.store.SessionByID(ctx, hashSecret(cookie))
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, nil, err
	}

	now := s.now().UTC()
	if now.After(sess.ExpiresAt) {
		_ = s.store.DeleteSession(ctx, sess.ID)
		return nil, nil, ErrSessionInvalid
	}
	if s.idleTTL > 0 && now.Sub(sess.LastSeenAt) > s.idleTTL {
		_ = s.store.DeleteSession(ctx, sess.ID)
		return nil, nil, ErrSessionInvalid
	}

	u, err := s.store.UserByID(ctx, sess.UserID)
	if errors.Is(err, db.ErrNotFound) {
		_ = s.store.DeleteSession(ctx, sess.ID)
		return nil, nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, nil, err
	}
	if u.Suspended {
		_ = s.store.DeleteUserSessions(ctx, u.ID)
		return nil, nil, ErrSuspended
	}

	// Only write when the clock has moved enough to matter, so a burst of
	// requests does not become a burst of writes on a single-writer store.
	if now.Sub(sess.LastSeenAt) > time.Minute {
		_ = s.store.TouchSession(ctx, sess.ID, now)
		sess.LastSeenAt = now
	}
	return u, sess, nil
}

// Logout destroys one session.
func (s *Service) Logout(ctx context.Context, cookie string) error {
	if cookie == "" {
		return nil
	}
	return s.store.DeleteSession(ctx, hashSecret(cookie))
}

// PurgeExpiredSessions removes rows past their absolute expiry.
func (s *Service) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	return s.store.PurgeExpiredSessions(ctx, s.now().UTC())
}

const tokenNamespace = "opanel"

// IssueAPIToken creates a token and returns its plaintext exactly once.
func (s *Service) IssueAPIToken(ctx context.Context, userID int64, name string, scopes []Scope, expiresAt *time.Time) (*db.APIToken, string, error) {
	for _, sc := range scopes {
		if !ValidScope(sc) {
			return nil, "", fmt.Errorf("auth: unknown scope %q", sc)
		}
	}
	// Hex, not base64url: the prefix is the field the parser splits on, so
	// it must never contain "_" or "-".
	prefix, err := randomHex(6) // 12 hex chars
	if err != nil {
		return nil, "", err
	}
	secret, err := randomToken(32)
	if err != nil {
		return nil, "", err
	}
	plain := tokenNamespace + "_" + prefix + "_" + secret

	strs := make([]string, len(scopes))
	for i, sc := range scopes {
		strs[i] = string(sc)
	}
	t, err := s.store.CreateAPIToken(ctx, &db.APIToken{
		Name:      name,
		UserID:    userID,
		Prefix:    prefix,
		TokenHash: hashSecret(secret),
		Scopes:    strs,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, "", err
	}
	return t, plain, nil
}

// AuthenticateToken resolves a bearer token to its user and record.
func (s *Service) AuthenticateToken(ctx context.Context, plain string) (*db.User, *db.APIToken, error) {
	// SplitN with 3: the secret is base64url and may itself contain "_",
	// so only the first two separators are structural.
	parts := strings.SplitN(plain, "_", 3)
	if len(parts) != 3 || parts[0] != tokenNamespace || parts[1] == "" || parts[2] == "" {
		return nil, nil, ErrTokenInvalid
	}
	t, err := s.store.APITokenByPrefix(ctx, parts[1])
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil, ErrTokenInvalid
	}
	if err != nil {
		return nil, nil, err
	}
	if !secretsEqual(t.TokenHash, hashSecret(parts[2])) {
		return nil, nil, ErrTokenInvalid
	}

	now := s.now().UTC()
	if t.RevokedAt != nil || (t.ExpiresAt != nil && now.After(*t.ExpiresAt)) {
		return nil, nil, ErrTokenInvalid
	}

	u, err := s.store.UserByID(ctx, t.UserID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil, ErrTokenInvalid
	}
	if err != nil {
		return nil, nil, err
	}
	if u.Suspended {
		return nil, nil, ErrSuspended
	}

	if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) > time.Minute {
		_ = s.store.TouchAPIToken(ctx, t.ID, now)
	}
	return u, t, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
