package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
)

// maxSFTPAccounts caps how many extra credentials one hosting account may
// hold. Not a licensing limit: every one of these is a Linux account and a
// line in the password file, and an interface that lets somebody make
// thousands by holding down a button is one that will be used to.
const maxSFTPAccounts = 20

// sftpLabel is the part of the name the customer chooses. The full Linux
// account is "<owner>_<label>", so the owner can always tell at a glance
// whose credential a name in /etc/passwd is.
var sftpLabel = regexp.MustCompile(`^[a-z][a-z0-9]{1,15}$`)

type sftpAccountView struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Owner     string `json:"owner"`
	Note      string `json:"note"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func viewSFTPAccount(a *db.SFTPAccount) sftpAccountView {
	return sftpAccountView{
		ID:        a.ID,
		Username:  a.Username,
		Owner:     a.Owner,
		Note:      a.Note,
		CreatedAt: a.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: a.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// sftpOwner resolves whose credentials are being asked about, and refuses a
// request for somebody else's unless the caller is staff over them.
//
// It also refuses an owner with no Linux account. An SFTP credential shares
// the owner's home and group; without one there is nothing to share, which
// is the case for every administrator and reseller.
func (s *Server) sftpOwner(w http.ResponseWriter, r *http.Request) *db.User {
	actor := userFrom(r.Context())
	target := actor

	if id := r.URL.Query().Get("user"); id != "" {
		other := s.userByIDParam(w, r, id)
		if other == nil {
			return nil
		}
		if other.ID != actor.ID && !auth.CanManage(actor, other) {
			writeError(w, http.StatusNotFound, "not_found", "no such user")
			return nil
		}
		target = other
	}

	if target.LinuxUID == nil || *target.LinuxUID == 0 {
		writeError(w, http.StatusBadRequest, "no_linux_account",
			"this account has no files on the server yet")
		return nil
	}
	return target
}

// handleFileAccessEnable gives the signed-in account a Linux user.
//
// Staff do not get one when they are created, because they own no websites.
// They ask for one when they want somewhere on the server to work: a shell
// to run curl in, a place to put a backup, a credential to upload with.
func (s *Server) handleFileAccessEnable(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if u.LinuxUID != nil && *u.LinuxUID != 0 {
		writeError(w, http.StatusConflict, "exists", "this account already has file access")
		return
	}
	password, err := s.users.EnableFileAccess(r.Context(), u.ID)
	if err != nil {
		s.audit(r, "account.file_access", u.Username, false, err.Error())
		writeError(w, http.StatusBadRequest, "failed", err.Error())
		return
	}
	s.audit(r, "account.file_access", u.Username, true, "")
	writeJSON(w, http.StatusCreated, map[string]any{
		"username": u.Username,
		"password": password,
		"host":     s.sftpHost(r),
		"port":     22,
	})
}

// handleSFTPPrimaryPassword resets the password on the account's own login.
//
// The extra credentials each had a reset button and the login the account is
// actually named after did not, which left the one everybody uses as the
// only one that could not be changed without asking staff.
func (s *Server) handleSFTPPrimaryPassword(w http.ResponseWriter, r *http.Request) {
	owner := s.sftpOwner(w, r)
	if owner == nil {
		return
	}
	password, err := s.users.SetSFTPPassword(r.Context(), owner.ID)
	if err != nil {
		s.audit(r, "sftp.password", owner.Username, false, err.Error())
		writeError(w, http.StatusBadRequest, "failed", err.Error())
		return
	}
	s.audit(r, "sftp.password", owner.Username, true, "own account")
	writeJSON(w, http.StatusOK, map[string]any{
		"username": owner.Username,
		"password": password,
		"host":     s.sftpHost(r),
		"port":     22,
	})
}

func (s *Server) handleSFTPList(w http.ResponseWriter, r *http.Request) {
	owner := s.sftpOwner(w, r)
	if owner == nil {
		return
	}
	rows, err := s.db.ListSFTPAccounts(r.Context(), owner.ID)
	if err != nil {
		s.log.Error("httpapi: list sftp accounts", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]sftpAccountView, 0, len(rows)+1)
	for _, a := range rows {
		out = append(out, viewSFTPAccount(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		// The owner's own account is the first credential and is not in the
		// table: it is not revocable, so it is reported separately rather
		// than listed beside things that are.
		"primary": owner.Username,
		"host":    s.sftpHost(r),
		"port":    22,
		"limit":   maxSFTPAccounts,
		"rows":    out,
	})
}

type createSFTPRequest struct {
	Label string `json:"label"`
	Note  string `json:"note"`
}

func (s *Server) handleSFTPCreate(w http.ResponseWriter, r *http.Request) {
	owner := s.sftpOwner(w, r)
	if owner == nil {
		return
	}
	var req createSFTPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !sftpLabel.MatchString(req.Label) {
		writeError(w, http.StatusBadRequest, "bad_label",
			"a name is 2 to 16 characters, lowercase letters and digits, starting with a letter")
		return
	}

	n, err := s.db.CountSFTPAccounts(r.Context(), owner.ID)
	if err != nil {
		s.log.Error("httpapi: count sftp accounts", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if n >= maxSFTPAccounts {
		writeError(w, http.StatusConflict, "limit_reached",
			"this account already has "+strconv.Itoa(maxSFTPAccounts)+" SFTP credentials")
		return
	}

	username := owner.Username + "_" + req.Label
	if !linuxuser.ValidName(username) {
		writeError(w, http.StatusBadRequest, "bad_label",
			"with the account prefix that name is too long for a Linux account")
		return
	}
	if _, err := s.db.SFTPAccountByName(r.Context(), username); err == nil {
		writeError(w, http.StatusConflict, "exists", username+" already exists")
		return
	} else if !errors.Is(err, db.ErrNotFound) {
		s.log.Error("httpapi: sftp account lookup", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	// The Linux account first. A row with no account behind it is a
	// credential that does not work; an account with no row is one nobody
	// can see to remove, which is worse.
	if _, err := agentclient.Call[linuxuser.Account](r.Context(), s.agent, "linuxuser.create_sftp", 1,
		actions.SFTPCredentialRequest{Username: username, Owner: owner.Username}); err != nil {
		s.audit(r, "sftp.create", username, false, err.Error())
		s.agentError(w, err)
		return
	}

	password, err := auth.NewRecoveryCodes(1)
	if err == nil {
		_, err = agentclient.Call[struct{}](r.Context(), s.agent, "linuxuser.set_password", 1,
			actions.AccountPasswordRequest{Username: username, Password: password[0]})
	}
	if err != nil {
		// Roll the account back: one with no password set cannot be used, but
		// leaving it behind means the name is taken for ever.
		if _, derr := agentclient.Call[struct{}](r.Context(), s.agent, "linuxuser.delete", 1,
			actions.AccountDeleteRequest{Username: username, RemoveHome: false}); derr != nil {
			s.log.Error("httpapi: could not remove a half-made sftp credential",
				"user", username, "err", derr)
		}
		s.audit(r, "sftp.create", username, false, err.Error())
		s.agentError(w, err)
		return
	}

	rec, err := s.db.CreateSFTPAccount(r.Context(), &db.SFTPAccount{
		OwnerID:  owner.ID,
		Username: username,
		Note:     req.Note,
	})
	if err != nil {
		s.log.Error("httpapi: record sftp account", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "sftp.create", username, true, "")

	writeJSON(w, http.StatusCreated, map[string]any{
		"account":  viewSFTPAccount(rec),
		"password": password[0],
		"host":     s.sftpHost(r),
		"port":     22,
	})
}

// loadSFTPAccount reads the credential named in the path and checks the
// caller is allowed to touch it.
func (s *Server) loadSFTPAccount(w http.ResponseWriter, r *http.Request) *db.SFTPAccount {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such credential")
		return nil
	}
	rec, err := s.db.SFTPAccountByID(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such credential")
		return nil
	}
	if err != nil {
		s.log.Error("httpapi: sftp account", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return nil
	}

	actor := userFrom(r.Context())
	if rec.OwnerID != actor.ID {
		owner, err := s.db.UserByID(r.Context(), rec.OwnerID)
		if err != nil || !auth.CanManage(actor, owner) {
			writeError(w, http.StatusNotFound, "not_found", "no such credential")
			return nil
		}
	}
	return rec
}

func (s *Server) handleSFTPPassword(w http.ResponseWriter, r *http.Request) {
	rec := s.loadSFTPAccount(w, r)
	if rec == nil {
		return
	}
	password, err := auth.NewRecoveryCodes(1)
	if err == nil {
		_, err = agentclient.Call[struct{}](r.Context(), s.agent, "linuxuser.set_password", 1,
			actions.AccountPasswordRequest{Username: rec.Username, Password: password[0]})
	}
	if err != nil {
		s.audit(r, "sftp.password", rec.Username, false, err.Error())
		s.agentError(w, err)
		return
	}
	if err := s.db.TouchSFTPAccount(r.Context(), rec.ID); err != nil {
		s.log.Warn("httpapi: could not record sftp password change", "err", err)
	}
	s.audit(r, "sftp.password", rec.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"username": rec.Username,
		"password": password[0],
		"host":     s.sftpHost(r),
		"port":     22,
	})
}

func (s *Server) handleSFTPDelete(w http.ResponseWriter, r *http.Request) {
	rec := s.loadSFTPAccount(w, r)
	if rec == nil {
		return
	}
	// RemoveHome is false and must stay false: the home belongs to the owner,
	// not to this credential, and userdel would take the whole account's
	// files with it.
	if _, err := agentclient.Call[struct{}](r.Context(), s.agent, "linuxuser.delete", 1,
		actions.AccountDeleteRequest{Username: rec.Username, RemoveHome: false}); err != nil {
		s.audit(r, "sftp.delete", rec.Username, false, err.Error())
		s.agentError(w, err)
		return
	}
	if err := s.db.DeleteSFTPAccount(r.Context(), rec.ID); err != nil {
		s.log.Error("httpapi: delete sftp account", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "sftp.delete", rec.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// sftpHost is the address to put in a client, which is the one the panel is
// being reached on. Falling back to the request's own host means a panel
// behind a name gives out that name rather than an address that may only
// work from where the server happens to be.
func (s *Server) sftpHost(r *http.Request) string {
	info, err := agentclient.Call[actions.NetworkInfo](r.Context(), s.agent, "net.info", 1, struct{}{})
	if err == nil && info.PrimaryIPv4 != "" {
		return info.PrimaryIPv4
	}
	host := r.Host
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}
