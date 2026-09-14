package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/pquerna/otp/totp"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/panelusers"
)

type panelUserView struct {
	ID          int64     `json:"id"`
	Username    string    `json:"username"`
	Email       string    `json:"email"`
	Role        string    `json:"role"`
	Suspended   bool      `json:"suspended"`
	TOTPEnabled bool      `json:"totp_enabled"`
	LinuxUID    *int64    `json:"linux_uid,omitempty"`
	Sites       int       `json:"sites"`
	Databases   int       `json:"databases"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Server) viewPanelUser(r *http.Request, u *db.User) panelUserView {
	v := panelUserView{
		ID: u.ID, Username: u.Username, Email: u.Email, Role: u.Role,
		Suspended: u.Suspended, TOTPEnabled: u.TOTPEnabled,
		LinuxUID: u.LinuxUID, CreatedAt: u.CreatedAt,
	}
	if sites, dbs, err := s.db.UserResourceCounts(r.Context(), u.ID); err == nil {
		v.Sites, v.Databases = sites, dbs
	}
	return v
}

func (s *Server) handleUserList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.ListUsers(r.Context(), auth.ScopeFor(userFrom(r.Context())))
	if err != nil {
		s.log.Error("httpapi: list users", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]panelUserView, 0, len(rows))
	for _, u := range rows {
		out = append(out, s.viewPanelUser(r, u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out, "roles": auth.AllRoles})
}

type createUserRequest struct {
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	Role     string `json:"role"`
	Password string `json:"password,omitempty"`
	// PlanID assigns a package at creation, which is also what the
	// reseller allowance is measured against.
	PlanID *int64 `json:"plan_id,omitempty"`
	// ParentID lets an administrator hand the account to a reseller.
	ParentID int64 `json:"parent_id,omitempty"`
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// A reseller may create end users but not staff, or the role boundary
	// would be one API call wide.
	actor := userFrom(r.Context())
	role := auth.Role(req.Role)
	if !auth.Role(actor.Role).AtLeast(auth.RoleAdmin) && role != auth.RoleEndUser {
		writeError(w, http.StatusForbidden, "forbidden", "only an administrator may create staff accounts")
		return
	}

	// A customer a reseller creates belongs to that reseller. An
	// administrator may hand the account to a reseller explicitly, and
	// creates it for the server itself when they do not.
	parentID := req.ParentID
	if auth.Role(actor.Role) == auth.RoleReseller {
		parentID = actor.ID
	}

	// The allowance is checked against the package being assigned, so a
	// reseller cannot sell 10 GB packages past the total they were given.
	planDisk := 0
	if req.PlanID != nil {
		plan, err := s.db.PlanByID(r.Context(), *req.PlanID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "no such package")
			return
		}
		planDisk = plan.DiskQuotaMB
	}
	if err := s.users.CheckCanCreateAccount(r.Context(), actor, planDisk); err != nil {
		s.audit(r, "user.create", req.Username, false, err.Error())
		writeError(w, http.StatusConflict, "allocation_exceeded", err.Error())
		return
	}

	res, err := s.users.Create(r.Context(), panelusers.CreateRequest{
		Username: req.Username, Email: req.Email, Role: role,
		Password: req.Password, ParentID: parentID,
	})
	if err != nil {
		s.audit(r, "user.create", req.Username, false, err.Error())
		s.userError(w, err)
		return
	}
	if req.PlanID != nil {
		if err := s.db.SetUserPlan(r.Context(), res.User.ID, req.PlanID); err != nil {
			s.log.Error("httpapi: assign package to new user", "err", err)
		}
	}
	// The filesystem limit goes on straight away. An account created with a
	// package and no quota behind it can fill the disk before anybody
	// notices the two were never connected.
	if err := s.plans.ApplyQuota(r.Context(), res.User); err != nil {
		s.log.Warn("httpapi: could not apply the disk quota for a new account",
			"user", res.User.Username, "err", err)
	}
	s.audit(r, "user.create", res.User.Username, true, string(role))
	writeJSON(w, http.StatusCreated, map[string]any{
		"user":          s.viewPanelUser(r, res.User),
		"password":      res.Password,
		"sftp_password": res.SFTPPassword,
	})
}

func (s *Server) loadPanelUser(w http.ResponseWriter, r *http.Request) *db.User {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "user id must be a number")
		return nil
	}
	u, err := s.db.UserByID(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return nil
	}
	if err != nil {
		s.log.Error("httpapi: load user", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return nil
	}
	// Not found rather than forbidden: telling a reseller that a username
	// they guessed exists on the server is itself a leak.
	if !auth.CanSee(userFrom(r.Context()), u) {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return nil
	}
	return u
}

// requireManage refuses a change the caller may see but not make. A reseller
// may manage their own end users and nobody else -- not another reseller's
// customer, not another reseller, and not an administrator.
func (s *Server) requireManage(w http.ResponseWriter, r *http.Request, target *db.User) bool {
	if auth.CanManage(userFrom(r.Context()), target) {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden",
		"that account is not yours to manage")
	return false
}

type updateUserRequest struct {
	Email     *string `json:"email,omitempty"`
	Role      *string `json:"role,omitempty"`
	Suspended *bool   `json:"suspended,omitempty"`
}

func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	target := s.loadPanelUser(w, r)
	if target == nil {
		return
	}
	var req updateUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	actor := userFrom(r.Context())
	if !s.requireManage(w, r, target) {
		return
	}

	if req.Suspended != nil {
		// Suspending yourself ends your own session mid-request and leaves
		// nobody able to undo it if you were the only administrator.
		if target.ID == actor.ID {
			writeError(w, http.StatusBadRequest, "bad_request", "you cannot suspend your own account")
			return
		}
		if err := s.users.SetSuspended(r.Context(), target.ID, *req.Suspended); err != nil {
			s.audit(r, "user.suspend", target.Username, false, err.Error())
			s.userError(w, err)
			return
		}
		s.audit(r, "user.suspend", target.Username, true, strconv.FormatBool(*req.Suspended))
	}

	if req.Email != nil || req.Role != nil {
		email := target.Email
		if req.Email != nil {
			email = *req.Email
		}
		role := auth.Role(target.Role)
		if req.Role != nil {
			role = auth.Role(*req.Role)
			if !auth.Role(actor.Role).AtLeast(auth.RoleAdmin) {
				writeError(w, http.StatusForbidden, "forbidden", "only an administrator may change a role")
				return
			}
			if target.ID == actor.ID && role != auth.RoleAdmin {
				writeError(w, http.StatusBadRequest, "bad_request", "you cannot demote your own account")
				return
			}
		}
		if err := s.users.UpdateProfile(r.Context(), target.ID, email, role); err != nil {
			s.audit(r, "user.update", target.Username, false, err.Error())
			s.userError(w, err)
			return
		}
		s.audit(r, "user.update", target.Username, true, "")
	}

	updated, err := s.db.UserByID(r.Context(), target.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, s.viewPanelUser(r, updated))
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	target := s.loadPanelUser(w, r)
	if target == nil {
		return
	}
	actor := userFrom(r.Context())
	if target.ID == actor.ID {
		writeError(w, http.StatusBadRequest, "bad_request", "you cannot delete your own account")
		return
	}
	if !s.requireManage(w, r, target) {
		return
	}
	removeFiles := r.URL.Query().Get("remove_files") == "true"

	if err := s.users.Delete(r.Context(), target.ID, removeFiles); err != nil {
		s.audit(r, "user.delete", target.Username, false, err.Error())
		s.userError(w, err)
		return
	}
	s.audit(r, "user.delete", target.Username, true,
		map[bool]string{true: "home removed", false: "home kept"}[removeFiles])
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type passwordRequest struct {
	Password string `json:"password,omitempty"`
}

func (s *Server) handleUserPassword(w http.ResponseWriter, r *http.Request) {
	target := s.loadPanelUser(w, r)
	if target == nil {
		return
	}
	var req passwordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	generated, err := s.users.SetPassword(r.Context(), target.ID, req.Password)
	if err != nil {
		s.audit(r, "user.password", target.Username, false, err.Error())
		s.userError(w, err)
		return
	}
	s.audit(r, "user.password", target.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"password": generated})
}

func (s *Server) handleUserSFTPPassword(w http.ResponseWriter, r *http.Request) {
	target := s.loadPanelUser(w, r)
	if target == nil {
		return
	}
	password, err := s.users.SetSFTPPassword(r.Context(), target.ID)
	if err != nil {
		s.audit(r, "user.sftp_password", target.Username, false, err.Error())
		s.userError(w, err)
		return
	}
	s.audit(r, "user.sftp_password", target.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"password": password})
}

// --- own account -----------------------------------------------------------

type changeOwnPasswordRequest struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (s *Server) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	var req changeOwnPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u := userFrom(r.Context())
	// The current password is required even though the session already
	// proves identity: it stops a borrowed, unlocked browser from becoming a
	// permanent takeover.
	if err := auth.VerifyPassword(u.PasswordHash, req.Current); err != nil {
		s.audit(r, "user.change_password", u.Username, false, "current password rejected")
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "current password is wrong")
		return
	}
	if len(req.New) < 10 {
		writeError(w, http.StatusBadRequest, "bad_request", "new password must be at least 10 characters")
		return
	}
	if _, err := s.users.SetPassword(r.Context(), u.ID, req.New); err != nil {
		s.userError(w, err)
		return
	}
	s.audit(r, "user.change_password", u.Username, true, "")
	// Every session was just revoked, including this one.
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "signed_out": true})
}

// --- two-factor -------------------------------------------------------------

// handleTOTPSetup returns a secret and recovery codes without turning
// anything on. Two-factor is only enabled once the user proves the
// authenticator works, so a mis-scanned QR code cannot lock them out.
func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u.TOTPEnabled {
		writeError(w, http.StatusConflict, "already_enabled", "two-factor is already enabled")
		return
	}
	setup, err := auth.NewTOTPSetup("OPanel", u.Username)
	if err != nil {
		s.log.Error("httpapi: totp setup", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// Stored but not enabled: the flag is what gates the login check.
	if err := s.db.SetTOTP(r.Context(), u.ID, setup.Secret, false); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.db.ReplaceRecoveryCodes(r.Context(), u.ID,
		auth.HashRecoveryCodes(setup.RecoveryCodes)); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"secret":         setup.Secret,
		"uri":            setup.URI,
		"recovery_codes": setup.RecoveryCodes,
	})
}

type totpCodeRequest struct {
	Code string `json:"code"`
}

func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	var req totpCodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u := userFrom(r.Context())
	if u.TOTPSecret == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "start with /auth/2fa/setup")
		return
	}
	if !totp.Validate(req.Code, u.TOTPSecret) {
		s.audit(r, "user.2fa.enable", u.Username, false, "code rejected")
		writeError(w, http.StatusBadRequest, "totp_invalid", "that code is not valid — check the clock on your device")
		return
	}
	if err := s.db.SetTOTP(r.Context(), u.ID, u.TOTPSecret, true); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "user.2fa.enable", u.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type disableTOTPRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	var req disableTOTPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u := userFrom(r.Context())
	// Turning off a second factor is exactly what an attacker on a borrowed
	// session would do first, so it costs a password.
	if err := auth.VerifyPassword(u.PasswordHash, req.Password); err != nil {
		s.audit(r, "user.2fa.disable", u.Username, false, "password rejected")
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "password is wrong")
		return
	}
	if err := s.db.SetTOTP(r.Context(), u.ID, "", false); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.db.ReplaceRecoveryCodes(r.Context(), u.ID, nil); err != nil {
		s.log.Warn("httpapi: could not clear recovery codes", "err", err)
	}
	s.audit(r, "user.2fa.disable", u.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) userError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, panelusers.ErrUsernameTaken):
		writeError(w, http.StatusConflict, "username_taken", err.Error())
	case errors.Is(err, panelusers.ErrBadUsername):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, panelusers.ErrLastAdmin):
		writeError(w, http.StatusBadRequest, "last_admin",
			"this is the last active administrator; promote another account first")
	case errors.Is(err, panelusers.ErrOwnsResources):
		writeError(w, http.StatusConflict, "owns_resources", err.Error())
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such user")
	default:
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}
