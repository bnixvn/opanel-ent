package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/databases"
	"github.com/bnixvn/opanel-ent/internal/db"
)

type databaseView struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	OwnerID   int64     `json:"owner_id"`
	Users     []string  `json:"users"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

type dbUserView struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Owner     string    `json:"owner"`
	OwnerID   int64     `json:"owner_id"`
	Databases []string  `json:"databases"`
	CreatedAt time.Time `json:"created_at"`
}

func viewDatabase(d *db.Database) databaseView {
	users := d.Users
	if users == nil {
		users = []string{}
	}
	return databaseView{
		ID: d.ID, Name: d.Name, Owner: d.OwnerUsername, OwnerID: d.OwnerID,
		Users: users, SizeBytes: d.SizeBytes, CreatedAt: d.CreatedAt,
	}
}

func viewDBUser(u *db.DBUser) dbUserView {
	names := u.Databases
	if names == nil {
		names = []string{}
	}
	return dbUserView{
		ID: u.ID, Username: u.Username, Owner: u.OwnerUsername, OwnerID: u.OwnerID,
		Databases: names, CreatedAt: u.CreatedAt,
	}
}

// ownerFor decides whose object a create request makes. Staff may name an
// owner; everyone else gets themselves whatever they sent.
func ownerFor(u *db.User, requested int64) int64 {
	if requested != 0 && auth.Role(u.Role).AtLeast(auth.RoleReseller) {
		return requested
	}
	return u.ID
}

func (s *Server) handleDatabaseList(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	rows, err := s.databases.ListDatabases(r.Context(), auth.ScopeFor(u))
	if err != nil {
		s.log.Error("httpapi: list databases", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]databaseView, 0, len(rows))
	for _, row := range rows {
		out = append(out, viewDatabase(row))
	}
	users, err := s.db.ListDBUsers(r.Context(), auth.ScopeFor(u))
	if err != nil {
		s.log.Error("httpapi: list database users", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	uout := make([]dbUserView, 0, len(users))
	for _, x := range users {
		uout = append(uout, viewDBUser(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"databases": out,
		"users":     uout,
		"host":      databases.ConnectionHost,
	})
}

type createDatabaseRequest struct {
	Name string `json:"name"`
	// Suffix is the same field under the name the interface uses; the panel
	// adds the owner prefix either way.
	Suffix  string `json:"suffix,omitempty"`
	OwnerID int64  `json:"owner_id,omitempty"`
	// WithoutUser skips the matching account, for the caller that wants to
	// attach an existing one instead.
	WithoutUser bool `json:"without_user,omitempty"`
}

func (s *Server) handleDatabaseCreate(w http.ResponseWriter, r *http.Request) {
	var req createDatabaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u := userFrom(r.Context())
	owner := ownerFor(u, req.OwnerID)
	suffix := req.Name
	if suffix == "" {
		suffix = req.Suffix
	}

	rec, err := s.databases.CreateDatabase(r.Context(), owner, suffix)
	if err != nil {
		s.audit(r, "database.create", suffix, false, err.Error())
		s.databaseError(w, err)
		return
	}
	s.audit(r, "database.create", rec.Name, true, "")

	// A database with nothing able to reach it is not a useful thing to have
	// created, and asking a customer to make an account and then grant it is
	// three screens for one intention. The account comes with it, and the
	// separate endpoints stay for the case that needs a second one -- a
	// read-only reporting login, say.
	if req.WithoutUser {
		writeJSON(w, http.StatusCreated, map[string]any{"database": viewDatabase(rec)})
		return
	}

	dbUser, password, err := s.databases.CreateUser(r.Context(), owner, suffix)
	if err != nil {
		// The database exists and is reported, because rolling it back would
		// throw away a name the customer may already have typed elsewhere.
		s.audit(r, "database.user.create", suffix, false, err.Error())
		writeJSON(w, http.StatusCreated, map[string]any{
			"database": viewDatabase(rec),
			"warning":  "the database was created but its account was not: " + err.Error(),
		})
		return
	}
	if err := s.databases.Grant(r.Context(), rec.ID, dbUser.ID); err != nil {
		s.audit(r, "database.grant", rec.Name, false, err.Error())
		writeJSON(w, http.StatusCreated, map[string]any{
			"database": viewDatabase(rec),
			"user":     viewDBUser(dbUser),
			"password": password,
			"warning":  "the account was created but not granted access: " + err.Error(),
		})
		return
	}
	s.audit(r, "database.user.create", dbUser.Username, true, "with "+rec.Name)

	writeJSON(w, http.StatusCreated, map[string]any{
		"database": viewDatabase(rec),
		"user":     viewDBUser(dbUser),
		"password": password,
		"host":     databases.ConnectionHost,
	})
}

// handleDatabaseExport streams a dump of one database to the browser.
func (s *Server) handleDatabaseExport(w http.ResponseWriter, r *http.Request) {
	_, rec := s.loadDatabase(w, r)
	if rec == nil {
		return
	}
	// Compressed unless asked otherwise: SQL text is about a tenth the size
	// gzipped, and the difference is minutes on a slow connection.
	compress := r.URL.Query().Get("compress") != "false"

	staged, err := agentclient.Call[actions.FileStageResult](r.Context(), s.agent, "db.export", 1,
		actions.DBExportRequest{Name: rec.Name, Compress: compress})
	if err != nil {
		s.audit(r, "database.export", rec.Name, false, err.Error())
		writeError(w, http.StatusInternalServerError, "export_failed", trimAgent(err.Error()))
		return
	}
	f, err := os.Open(staged.StagedPath)
	if err != nil {
		_ = os.Remove(staged.StagedPath)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(staged.StagedPath)
	}()

	name := actions.ExportFileName(rec.Name, compress)
	contentType := "application/sql"
	if compress {
		contentType = "application/gzip"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(staged.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	s.audit(r, "database.export", rec.Name, true, name)
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("httpapi: database download interrupted", "database", rec.Name, "err", err)
	}
}

func (s *Server) handleDatabaseDelete(w http.ResponseWriter, r *http.Request) {
	id, rec := s.loadDatabase(w, r)
	if rec == nil {
		return
	}
	if err := s.databases.DeleteDatabase(r.Context(), id); err != nil {
		s.audit(r, "database.delete", rec.Name, false, err.Error())
		s.databaseError(w, err)
		return
	}
	s.audit(r, "database.delete", rec.Name, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type createDBUserRequest struct {
	Username string `json:"username"`
	OwnerID  int64  `json:"owner_id,omitempty"`
}

func (s *Server) handleDBUserCreate(w http.ResponseWriter, r *http.Request) {
	var req createDBUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u := userFrom(r.Context())
	rec, password, err := s.databases.CreateUser(r.Context(), ownerFor(u, req.OwnerID), req.Username)
	if err != nil {
		s.audit(r, "database.user.create", req.Username, false, err.Error())
		s.databaseError(w, err)
		return
	}
	s.audit(r, "database.user.create", rec.Username, true, "")
	// The password is returned exactly once. It is not stored anywhere the
	// panel can read it back, so a lost password means a reset, not a lookup.
	writeJSON(w, http.StatusCreated, map[string]any{
		"user":     viewDBUser(rec),
		"password": password,
	})
}

func (s *Server) handleDBUserDelete(w http.ResponseWriter, r *http.Request) {
	id, rec := s.loadDBUser(w, r)
	if rec == nil {
		return
	}
	if err := s.databases.DeleteUser(r.Context(), id); err != nil {
		s.audit(r, "database.user.delete", rec.Username, false, err.Error())
		s.databaseError(w, err)
		return
	}
	s.audit(r, "database.user.delete", rec.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDBUserPassword(w http.ResponseWriter, r *http.Request) {
	id, rec := s.loadDBUser(w, r)
	if rec == nil {
		return
	}
	password, err := s.databases.ResetPassword(r.Context(), id)
	if err != nil {
		s.audit(r, "database.user.password", rec.Username, false, err.Error())
		s.databaseError(w, err)
		return
	}
	s.audit(r, "database.user.password", rec.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"password": password})
}

type grantRequest struct {
	UserID int64 `json:"user_id"`
}

func (s *Server) handleGrant(w http.ResponseWriter, r *http.Request) {
	id, rec := s.loadDatabase(w, r)
	if rec == nil {
		return
	}
	var req grantRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, dbUser := s.checkDBUserAccess(w, r, req.UserID); dbUser == nil {
		return
	}
	if err := s.databases.Grant(r.Context(), id, req.UserID); err != nil {
		s.audit(r, "database.grant", rec.Name, false, err.Error())
		s.databaseError(w, err)
		return
	}
	s.audit(r, "database.grant", rec.Name, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	id, rec := s.loadDatabase(w, r)
	if rec == nil {
		return
	}
	userID, err := strconv.ParseInt(chi.URLParam(r, "userID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "user id must be a number")
		return
	}
	if _, dbUser := s.checkDBUserAccess(w, r, userID); dbUser == nil {
		return
	}
	if err := s.databases.Revoke(r.Context(), id, userID); err != nil {
		s.audit(r, "database.revoke", rec.Name, false, err.Error())
		s.databaseError(w, err)
		return
	}
	s.audit(r, "database.revoke", rec.Name, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// loadDatabase resolves :id and enforces ownership, answering 404 rather than
// 403 for someone else's database so the endpoint cannot be used to discover
// which names exist.
func (s *Server) loadDatabase(w http.ResponseWriter, r *http.Request) (int64, *db.Database) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "database id must be a number")
		return 0, nil
	}
	rec, err := s.db.DatabaseByID(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such database")
		return 0, nil
	}
	if err != nil {
		s.log.Error("httpapi: load database", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return 0, nil
	}
	u := userFrom(r.Context())
	if !auth.Role(u.Role).AtLeast(auth.RoleReseller) && rec.OwnerID != u.ID {
		writeError(w, http.StatusNotFound, "not_found", "no such database")
		return 0, nil
	}
	return id, rec
}

func (s *Server) loadDBUser(w http.ResponseWriter, r *http.Request) (int64, *db.DBUser) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "user id must be a number")
		return 0, nil
	}
	return s.checkDBUserAccess(w, r, id)
}

func (s *Server) checkDBUserAccess(w http.ResponseWriter, r *http.Request, id int64) (int64, *db.DBUser) {
	rec, err := s.db.DBUserByID(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such database user")
		return 0, nil
	}
	if err != nil {
		s.log.Error("httpapi: load database user", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return 0, nil
	}
	u := userFrom(r.Context())
	if !auth.Role(u.Role).AtLeast(auth.RoleReseller) && rec.OwnerID != u.ID {
		writeError(w, http.StatusNotFound, "not_found", "no such database user")
		return 0, nil
	}
	return id, rec
}

func (s *Server) databaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, databases.ErrNameTaken):
		writeError(w, http.StatusConflict, "name_taken", err.Error())
	case errors.Is(err, databases.ErrOwnerNotFound):
		writeError(w, http.StatusBadRequest, "bad_request", "no such owner")
	case errors.Is(err, databases.ErrBadSuffix):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	default:
		var ae *agentclient.Error
		if errors.As(err, &ae) {
			s.agentError(w, err)
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}
