package httpapi

import (
	"net/http"
	"net/mail"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/auth"
)

// MaxEmailLength is generous: the standard allows 254 characters and a
// longer one is a mistake or an attempt to fill a column.
const MaxEmailLength = 254

// handleChangeOwnEmail sets the address on the caller's own account.
//
// The current password is required. The address is where certificate expiry
// warnings go and, in time, where a password reset would be sent, so being
// able to change it from an unlocked browser would be a way to take an
// account over quietly.
func (s *Server) handleChangeOwnEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	u := userFrom(r.Context())
	if err := auth.VerifyPassword(u.PasswordHash, req.Password); err != nil {
		s.audit(r, "user.change_email", u.Username, false, "password rejected")
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "your password is wrong")
		return
	}

	email, err := normalizeEmail(req.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.db.UpdateUserProfile(r.Context(), u.ID, email, u.Role); err != nil {
		s.log.Error("httpapi: change email", "user", u.Username, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "user.change_email", u.Username, true, email)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": email})
}

// normalizeEmail validates an address and lowercases it. An empty value is
// allowed and means "no contact address".
func normalizeEmail(raw string) (string, error) {
	e := strings.TrimSpace(raw)
	if e == "" {
		return "", nil
	}
	if len(e) > MaxEmailLength {
		return "", errTooLong
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e {
		// ParseAddress accepts `Name <a@b>`, which is not what belongs in
		// this field: it ends up in an ACME registration and in headers.
		return "", errBadEmail
	}
	return strings.ToLower(e), nil
}

type emailError string

func (e emailError) Error() string { return string(e) }

const (
	errTooLong  emailError = "that address is too long"
	errBadEmail emailError = "that does not look like an email address"
)

// acmeContact picks the address to register with the certificate authority.
//
// The site owner's, so expiry warnings reach whoever can act on them, then
// the operator making the request, then whatever the server was configured
// with. Nothing is invented: an ACME account with no contact is allowed, and
// an empty address is better than somebody else's.
func (s *Server) acmeContact(r *http.Request, ownerID int64) string {
	if ownerID != 0 {
		if owner, err := s.db.UserByID(r.Context(), ownerID); err == nil && owner.Email != "" {
			return owner.Email
		}
	}
	if u := userFrom(r.Context()); u != nil && u.Email != "" {
		return u.Email
	}
	return s.cfg.ACMEEmail
}
