package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

type ctxKey int

const (
	ctxKeyUser ctxKey = iota
	ctxKeyToken
	ctxKeySession
)

// SessionCookieName is the cookie holding the session secret.
const SessionCookieName = "opanel_session"

// userFrom returns the authenticated user, if any.
func userFrom(ctx context.Context) *db.User {
	u, _ := ctx.Value(ctxKeyUser).(*db.User)
	return u
}

// tokenFrom returns the API token used, when the caller authenticated with
// one rather than a session cookie.
func tokenFrom(ctx context.Context) *db.APIToken {
	t, _ := ctx.Value(ctxKeyToken).(*db.APIToken)
	return t
}

// sessionFrom returns the session backing the request, or nil for a request
// authenticated with a bearer token.
func sessionFrom(ctx context.Context) *db.Session {
	s, _ := ctx.Value(ctxKeySession).(*db.Session)
	return s
}

// impersonatorFrom returns the id of the member of staff driving this
// session, or 0 when the caller is signed in as themselves.
func impersonatorFrom(ctx context.Context) int64 {
	if s := sessionFrom(ctx); s != nil {
		return s.ImpersonatorID
	}
	return 0
}

// clientIP extracts the caller address. Proxy headers are deliberately
// ignored: the panel is reached directly, so trusting X-Forwarded-For would
// let any client spoof the value the rate limiter keys on.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// recoverer turns a panic in a handler into a 500 instead of killing the
// process and every in-flight request with it.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if errors.Is(r.Context().Err(), context.Canceled) {
					return // client hung up mid-write; not a real panic to report
				}
				s.log.Error("httpapi: panic",
					"err", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)
		s.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.status,
			"ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r),
		)
	})
}

// securityHeaders applies defaults that matter for a panel served over TLS.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if s.cfg.SecureCookies() {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate accepts either a session cookie or a bearer token and rejects
// anything else. It never reveals which of the two was malformed.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if bearer := bearerToken(r); bearer != "" {
			u, tok, err := s.auth.AuthenticateToken(ctx, bearer)
			if err != nil {
				s.rejectAuth(w, err)
				return
			}
			ctx = context.WithValue(ctx, ctxKeyUser, u)
			ctx = context.WithValue(ctx, ctxKeyToken, tok)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		c, err := r.Cookie(SessionCookieName)
		if err != nil || c.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
			return
		}
		u, sess, err := s.auth.ValidateSession(ctx, c.Value)
		if err != nil {
			s.clearSessionCookie(w)
			s.rejectAuth(w, err)
			return
		}
		ctx = context.WithValue(ctx, ctxKeyUser, u)
		ctx = context.WithValue(ctx, ctxKeySession, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) rejectAuth(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrSuspended):
		writeError(w, http.StatusForbidden, "suspended", "account is suspended")
	case errors.Is(err, auth.ErrSessionInvalid), errors.Is(err, auth.ErrTokenInvalid):
		writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
	default:
		s.log.Error("httpapi: authentication error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// roleAtLeastReseller is the staff test the handlers share.
func roleAtLeastReseller(u *db.User) bool {
	return auth.Role(u.Role).AtLeast(auth.RoleReseller)
}

// requireRole gates a route on a minimum role.
func (s *Server) requireRole(min auth.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := userFrom(r.Context())
			if u == nil {
				writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
				return
			}
			if !auth.Role(u.Role).AtLeast(min) {
				writeError(w, http.StatusForbidden, "forbidden", "insufficient role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireScope gates a route used by machine callers. A session-authenticated
// request carries no token and passes on its role alone; a token must name
// the scope explicitly.
func (s *Server) requireScope(scope auth.Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := tokenFrom(r.Context())
			if tok == nil {
				next.ServeHTTP(w, r)
				return
			}
			for _, s := range tok.Scopes {
				if s == string(scope) {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeError(w, http.StatusForbidden, "missing_scope",
				"token lacks required scope "+string(scope))
		})
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}
