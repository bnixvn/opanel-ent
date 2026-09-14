package auth

import (
	"context"
	"errors"

	"github.com/bnixvn/opanel-ent/internal/db"
)

// ErrNotImpersonating is returned when a session is asked to hand control
// back but was never handed control in the first place.
var ErrNotImpersonating = errors.New("auth: this session is not an impersonation")

// Impersonate opens a session belonging to target, driven by actor.
//
// The session belongs to the target rather than carrying a flag on the
// operator's own session, because every permission check in the panel reads
// the session's user. Making the customer the session's user is what makes
// "log in as" show exactly what the customer sees -- including what they
// cannot do -- rather than an approximation of it.
//
// Whether actor may do this is decided by the caller: this package knows
// about sessions, not about which accounts a reseller owns.
func (s *Service) Impersonate(ctx context.Context, actor, target *db.User, ip, userAgent string) (*db.Session, string, error) {
	if target.Suspended {
		return nil, "", ErrSuspended
	}
	return s.newSessionAs(ctx, target.ID, actor.ID, ip, userAgent)
}

// StopImpersonating ends an impersonated session and opens a fresh one for
// the operator who started it.
//
// The operator is not asked to sign in again: this session exists only
// because they were authenticated when they started, and the record of who
// that was is on the session itself, not supplied by the caller.
func (s *Service) StopImpersonating(ctx context.Context, cookie, ip, userAgent string) (*db.User, *db.Session, string, error) {
	sess, err := s.store.SessionByID(ctx, hashSecret(cookie))
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil, "", ErrSessionInvalid
	}
	if err != nil {
		return nil, nil, "", err
	}
	if sess.ImpersonatorID == 0 {
		return nil, nil, "", ErrNotImpersonating
	}

	operator, err := s.store.UserByID(ctx, sess.ImpersonatorID)
	if errors.Is(err, db.ErrNotFound) {
		// The operator's account is gone. There is nothing to return to, so
		// the session ends rather than becoming an orphan with a customer's
		// permissions and no owner.
		_ = s.store.DeleteSession(ctx, sess.ID)
		return nil, nil, "", ErrSessionInvalid
	}
	if err != nil {
		return nil, nil, "", err
	}
	if operator.Suspended {
		_ = s.store.DeleteSession(ctx, sess.ID)
		return nil, nil, "", ErrSuspended
	}

	// Order matters: open the replacement first, so a failure between the
	// two leaves the operator with a working session rather than none.
	next, fresh, err := s.newSessionAs(ctx, operator.ID, 0, ip, userAgent)
	if err != nil {
		return nil, nil, "", err
	}
	if err := s.store.DeleteSession(ctx, sess.ID); err != nil {
		return nil, nil, "", err
	}
	return operator, next, fresh, nil
}

// StartSession opens an ordinary session for an account that has already
// been authenticated by some other means.
//
// Passkey sign-in is the caller: the assertion is the proof, and there is no
// password to verify. The suspension check is repeated here rather than left
// to the caller, because this is the function that mints the session and a
// suspended account must never get one.
func (s *Service) StartSession(ctx context.Context, u *db.User, ip, userAgent string) (*db.Session, string, error) {
	if u.Suspended {
		return nil, "", ErrSuspended
	}
	return s.newSessionAs(ctx, u.ID, 0, ip, userAgent)
}

// SessionFor resolves a cookie to its session row without the freshness
// checks ValidateSession makes. Callers that already validated the request
// use it to read what the session is, not whether it is good.
func (s *Service) SessionFor(ctx context.Context, cookie string) (*db.Session, error) {
	if cookie == "" {
		return nil, ErrSessionInvalid
	}
	sess, err := s.store.SessionByID(ctx, hashSecret(cookie))
	if errors.Is(err, db.ErrNotFound) {
		return nil, ErrSessionInvalid
	}
	return sess, err
}
