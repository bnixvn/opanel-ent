package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Code     string `json:"code,omitempty"` // TOTP or recovery code
}

type userView struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	TOTPEnabled bool   `json:"totp_enabled"`
}

func viewUser(u *db.User) userView {
	return userView{
		ID: u.ID, Username: u.Username, Email: u.Email,
		Role: u.Role, TOTPEnabled: u.TOTPEnabled,
	}
}

type loginResponse struct {
	User      userView  `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	ip := clientIP(r)
	if ok, retryIn := s.loginLimiter.Allow(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryIn.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"too many attempts, try again shortly")
		return
	}

	res, err := s.auth.Login(r.Context(), req.Username, req.Password, req.Code, ip, r.UserAgent())
	if err != nil {
		s.audit(r, "auth.login", req.Username, false, err.Error())
		switch {
		case errors.Is(err, auth.ErrTOTPRequired):
			// Not a failure: the password was right and a second factor is
			// now expected. A distinct code lets the UI show the 2FA field.
			writeError(w, http.StatusUnauthorized, "totp_required", "two-factor code required")
		case errors.Is(err, auth.ErrTOTPInvalid):
			writeError(w, http.StatusUnauthorized, "totp_invalid", "invalid two-factor code")
		case errors.Is(err, auth.ErrSuspended):
			writeError(w, http.StatusForbidden, "suspended", "account is suspended")
		case errors.Is(err, auth.ErrInvalidCredentials):
			// Same response whether the username exists or not.
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
		default:
			s.log.Error("httpapi: login", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}

	s.loginLimiter.Reset(ip)
	s.setSessionCookie(w, res.Cookie, res.Session.ExpiresAt)
	s.audit(r, "auth.login", res.User.Username, true, "")
	writeJSON(w, http.StatusOK, loginResponse{
		User:      viewUser(res.User),
		ExpiresAt: res.Session.ExpiresAt,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if err := s.auth.Logout(r.Context(), c.Value); err != nil {
			s.log.Warn("httpapi: logout", "err", err)
		}
	}
	s.clearSessionCookie(w)
	s.audit(r, "auth.logout", "", true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, viewUser(u))
}

// audit records an action. Failures to write the trail are logged but never
// surfaced: losing an audit row must not turn a working request into an error
// the user sees.
func (s *Server) audit(r *http.Request, action, target string, ok bool, detail string) {
	e := db.AuditEntry{
		At:        time.Now().UTC(),
		ActorType: "anonymous",
		Action:    action,
		Target:    target,
		OK:        ok,
		Detail:    detail,
		IP:        clientIP(r),
	}
	if u := userFrom(r.Context()); u != nil {
		e.ActorType = "user"
		e.ActorID = &u.ID
		e.ActorName = u.Username
	}
	if t := tokenFrom(r.Context()); t != nil {
		e.ActorType = "token"
		e.ActorName = t.Name
	}
	if err := s.db.AppendAudit(r.Context(), e); err != nil {
		s.log.Warn("httpapi: append audit", "err", err, "action", action)
	}
}
