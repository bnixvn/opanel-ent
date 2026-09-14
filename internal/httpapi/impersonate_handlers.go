package httpapi

import (
	"errors"
	"net/http"

	"github.com/bnixvn/opanel-ent/internal/auth"
)

// handleImpersonate signs the operator in as another account.
//
// The session that comes back belongs to the customer, so the panel shows
// exactly what the customer sees, including what they are not allowed to do.
// Which accounts an operator may do this to is the same question as which
// accounts they may manage, so it is answered the same way.
func (s *Server) handleImpersonate(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r.Context())

	// Already wearing somebody else's account. Allowing a second hop would
	// make the audit trail a guess: the record says who started, and a chain
	// would leave no honest answer to "who did this".
	if impersonatorFrom(r.Context()) != 0 {
		writeError(w, http.StatusForbidden, "forbidden",
			"return to your own account before signing in as somebody else")
		return
	}
	// A bearer token has no session to return to, so there would be no way
	// out of the impersonation.
	if tokenFrom(r.Context()) != nil {
		writeError(w, http.StatusForbidden, "forbidden",
			"signing in as another account needs a browser session")
		return
	}

	target := s.loadPanelUser(w, r)
	if target == nil {
		return
	}
	if target.ID == actor.ID {
		writeError(w, http.StatusBadRequest, "bad_request", "that is your own account")
		return
	}
	if !auth.CanManage(actor, target) {
		writeError(w, http.StatusForbidden, "forbidden",
			"that account is not one you can sign in as")
		return
	}
	// Staff signing in as other staff would be a way around the rule that a
	// reseller cannot see another reseller's customers: the operator would
	// simply borrow an account that can.
	if auth.Role(target.Role).AtLeast(auth.RoleReseller) {
		writeError(w, http.StatusForbidden, "forbidden",
			"you can only sign in as a customer account")
		return
	}

	sess, cookie, err := s.auth.Impersonate(r.Context(), actor, target,
		clientIP(r), r.UserAgent())
	if errors.Is(err, auth.ErrSuspended) {
		writeError(w, http.StatusConflict, "suspended",
			"that account is suspended; unsuspend it first")
		return
	}
	if err != nil {
		s.audit(r, "user.impersonate", target.Username, false, err.Error())
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "user.impersonate", target.Username, true, "started")
	s.setSessionCookie(w, cookie, sess.ExpiresAt)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":            viewUser(target),
		"impersonated_by": actor.Username,
	})
}

// handleStopImpersonating hands the operator back their own session.
func (s *Server) handleStopImpersonating(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil || c.Value == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "no session to return from")
		return
	}
	target := userFrom(r.Context())

	operator, sess, cookie, err := s.auth.StopImpersonating(r.Context(), c.Value,
		clientIP(r), r.UserAgent())
	switch {
	case errors.Is(err, auth.ErrNotImpersonating):
		writeError(w, http.StatusBadRequest, "not_impersonating",
			"this session is your own account")
		return
	case errors.Is(err, auth.ErrSuspended), errors.Is(err, auth.ErrSessionInvalid):
		// The operator's account is gone or suspended, and the impersonated
		// session has been destroyed with it. There is nowhere to return to.
		s.clearSessionCookie(w)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in again")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	s.audit(r, "user.impersonate", target.Username, true, "ended by "+operator.Username)
	s.setSessionCookie(w, cookie, sess.ExpiresAt)
	writeJSON(w, http.StatusOK, map[string]any{"user": viewUser(operator)})
}

// refuseWhileImpersonating blocks the handful of actions that would let an
// operator change the customer's own credentials from inside their account.
//
// Staff can already reset a password from the users page, and that is
// recorded against the operator. Doing it from inside the customer's session
// would be recorded against the customer, which is the part that matters:
// the account holder has to be able to tell what they did from what was done
// to them.
func (s *Server) refuseWhileImpersonating(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if impersonatorFrom(r.Context()) != 0 {
			writeError(w, http.StatusForbidden, "impersonating",
				"this cannot be changed while signed in as another account")
			return
		}
		next.ServeHTTP(w, r)
	})
}
