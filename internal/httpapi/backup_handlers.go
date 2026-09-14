package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/backuparchive"
	"github.com/bnixvn/opanel-ent/internal/backups"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// MaxBackupUploadBytes caps an archive coming in from outside. Larger than a
// file upload because a whole account legitimately is larger, and because the
// alternative for a customer moving in from another host is no path at all.
const MaxBackupUploadBytes = 4 << 30 // 4 GiB

type backupView struct {
	ID         int64    `json:"id"`
	Owner      string   `json:"owner"`
	OwnerID    int64    `json:"owner_id"`
	Filename   string   `json:"filename"`
	Kind       string   `json:"kind"`
	Status     string   `json:"status"`
	Error      string   `json:"error,omitempty"`
	SizeBytes  int64    `json:"size_bytes"`
	FileCount  int64    `json:"file_count"`
	Databases  []string `json:"databases"`
	HasFiles   bool     `json:"has_files"`
	SHA256     string   `json:"sha256,omitempty"`
	CreatedAt  string   `json:"created_at"`
	FinishedAt string   `json:"finished_at,omitempty"`
}

func viewBackup(b *db.Backup) backupView {
	dbs := b.Databases
	if dbs == nil {
		dbs = []string{}
	}
	v := backupView{
		ID: b.ID, Owner: b.OwnerUsername, OwnerID: b.OwnerID,
		Filename: b.Filename, Kind: b.Kind, Status: b.Status, Error: b.Error,
		SizeBytes: b.SizeBytes, FileCount: b.FileCount, Databases: dbs,
		HasFiles: b.HasFiles, SHA256: b.SHA256,
		CreatedAt: b.CreatedAt.Format(time.RFC3339),
	}
	if !b.FinishedAt.IsZero() {
		v.FinishedAt = b.FinishedAt.Format(time.RFC3339)
	}
	return v
}

// backupOwner resolves the account a request is about, writing the error
// response itself and returning false when the caller must stop.
func (s *Server) backupOwner(w http.ResponseWriter, r *http.Request) (backups.Owner, bool) {
	o, err := s.backups.ResolveOwner(r.Context(), userFrom(r.Context()), r.URL.Query().Get("owner"))
	switch {
	case errors.Is(err, backups.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "you may only manage your own backups")
		return backups.Owner{}, false
	case errors.Is(err, backups.ErrNoAccount):
		writeError(w, http.StatusBadRequest, "no_account",
			"this account has nothing to back up; pick a hosting account with ?owner=")
		return backups.Owner{}, false
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return backups.Owner{}, false
	}
	return o, true
}

// loadBackup resolves the :id parameter and enforces ownership.
func (s *Server) loadBackup(w http.ResponseWriter, r *http.Request) *db.Backup {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "backup id must be a number")
		return nil
	}
	b, err := s.db.BackupByID(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such backup")
		return nil
	}
	if err != nil {
		s.log.Error("httpapi: load backup", "err", err, "backup_id", id)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return nil
	}
	if !backups.CanReach(userFrom(r.Context()), b) {
		// Not found rather than forbidden: confirming that somebody else's
		// backup exists is itself a small leak.
		writeError(w, http.StatusNotFound, "not_found", "no such backup")
		return nil
	}
	return b
}

func (s *Server) handleBackupList(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	scope := u.ID
	// A named owner is resolved for everyone, so an end user who asks for
	// somebody else's list is told no rather than quietly handed their own.
	if r.URL.Query().Get("owner") != "" {
		o, ok := s.backupOwner(w, r)
		if !ok {
			return
		}
		scope = o.ID
	} else if roleAtLeastReseller(u) {
		scope = 0 // staff see the whole server by default
	}
	rows, err := s.db.ListBackups(r.Context(), scope)
	if err != nil {
		s.log.Error("httpapi: list backups", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]backupView, 0, len(rows))
	for _, row := range rows {
		out = append(out, viewBackup(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backups":          out,
		"max_upload_bytes": int64(MaxBackupUploadBytes),
	})
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	o, ok := s.backupOwner(w, r)
	if !ok {
		return
	}
	req := struct {
		IncludeFiles     *bool `json:"include_files"`
		IncludeDatabases *bool `json:"include_databases"`
	}{}
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}
	// Everything, unless the caller said otherwise: a backup that quietly
	// left out the databases is the worst kind of backup.
	files, databases := true, true
	if req.IncludeFiles != nil {
		files = *req.IncludeFiles
	}
	if req.IncludeDatabases != nil {
		databases = *req.IncludeDatabases
	}

	row, err := s.backups.Start(r.Context(), backups.Request{
		Owner: o, IncludeFiles: files, IncludeDatabases: databases,
	})
	if errors.Is(err, backups.ErrBusy) {
		writeError(w, http.StatusConflict, "busy", "a backup for this account is already running")
		return
	}
	if err != nil {
		s.audit(r, "backup.create", o.Username, false, err.Error())
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.audit(r, "backup.create", o.Username+":"+row.Filename, true, "")
	writeJSON(w, http.StatusAccepted, map[string]any{"backup": viewBackup(row)})
}

func (s *Server) handleBackupGet(w http.ResponseWriter, r *http.Request) {
	b := s.loadBackup(w, r)
	if b == nil {
		return
	}
	out := map[string]any{"backup": viewBackup(b)}
	// The manifest is only worth asking the agent for once the archive
	// exists, and only says anything useful then.
	if b.Status == db.BackupReady {
		if m, err := s.backups.Inspect(r.Context(), b); err == nil {
			out["manifest"] = m
		} else {
			s.log.Warn("httpapi: cannot read backup manifest", "id", b.ID, "err", err)
			out["manifest_error"] = "the archive could not be read"
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	b := s.loadBackup(w, r)
	if b == nil {
		return
	}
	req := struct {
		Files     *bool `json:"files"`
		Databases *bool `json:"databases"`
	}{}
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}
	files, databases := true, true
	if req.Files != nil {
		files = *req.Files
	}
	if req.Databases != nil {
		databases = *req.Databases
	}
	if !files && !databases {
		writeError(w, http.StatusBadRequest, "bad_request",
			"choose the files, the databases, or both")
		return
	}

	res, err := s.backups.Restore(r.Context(), b, files, databases)
	if err != nil {
		s.audit(r, "backup.restore", b.OwnerUsername+":"+b.Filename, false, err.Error())
		s.log.Error("httpapi: restore failed", "id", b.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "restore_failed", trimAgent(err.Error()))
		return
	}
	s.audit(r, "backup.restore", b.OwnerUsername+":"+b.Filename, true,
		fmt.Sprintf("%d files, %d databases", res.Files, len(res.Databases)))
	writeJSON(w, http.StatusOK, map[string]any{
		"restored_files":     res.Files,
		"restored_databases": res.Databases,
		"manifest":           res.Manifest,
	})
}

func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	b := s.loadBackup(w, r)
	if b == nil {
		return
	}
	if err := s.backups.Delete(r.Context(), b); err != nil {
		s.audit(r, "backup.delete", b.OwnerUsername+":"+b.Filename, false, err.Error())
		writeError(w, http.StatusBadRequest, "bad_request", trimAgent(err.Error()))
		return
	}
	s.audit(r, "backup.delete", b.OwnerUsername+":"+b.Filename, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": b.Filename})
}

func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	b := s.loadBackup(w, r)
	if b == nil {
		return
	}
	f, err := s.backups.Download(r.Context(), b)
	if err != nil {
		s.log.Error("httpapi: stage backup", "id", b.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", trimAgent(err.Error()))
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.Name))
	if _, err := io.Copy(w, f.File); err != nil {
		s.log.Warn("httpapi: backup download interrupted", "id", b.ID, "err", err)
	}
}

func (s *Server) handleBackupUpload(w http.ResponseWriter, r *http.Request) {
	o, ok := s.backupOwner(w, r)
	if !ok {
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "expected a multipart upload")
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "malformed upload")
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			_ = part.Close()
			continue
		}
		if !strings.HasSuffix(path.Base(part.FileName()), backuparchive.Extension) {
			_ = part.Close()
			writeError(w, http.StatusBadRequest, "bad_request",
				"a backup archive must be a "+backuparchive.Extension+" file written by OPanel")
			return
		}
		row, err := s.backups.Upload(r.Context(), o, part, MaxBackupUploadBytes)
		_ = part.Close()
		if err != nil {
			s.audit(r, "backup.upload", o.Username, false, err.Error())
			writeError(w, http.StatusBadRequest, "bad_request", trimAgent(err.Error()))
			return
		}
		s.audit(r, "backup.upload", o.Username+":"+row.Filename, true, "")
		writeJSON(w, http.StatusCreated, map[string]any{"backup": viewBackup(row)})
		return
	}
	writeError(w, http.StatusBadRequest, "bad_request", "no file was included")
}

type scheduleView struct {
	Owner            string `json:"owner"`
	OwnerID          int64  `json:"owner_id"`
	Enabled          bool   `json:"enabled"`
	Frequency        string `json:"frequency"`
	Hour             int    `json:"hour"`
	Weekday          int    `json:"weekday"`
	Keep             int    `json:"keep"`
	IncludeFiles     bool   `json:"include_files"`
	IncludeDatabases bool   `json:"include_databases"`
	LastRunAt        string `json:"last_run_at,omitempty"`
	LastError        string `json:"last_error,omitempty"`
}

func viewSchedule(s *db.BackupSchedule) scheduleView {
	v := scheduleView{
		Owner: s.OwnerUsername, OwnerID: s.OwnerID, Enabled: s.Enabled,
		Frequency: s.Frequency, Hour: s.Hour, Weekday: s.Weekday, Keep: s.Keep,
		IncludeFiles: s.IncludeFiles, IncludeDatabases: s.IncludeDatabases,
		LastError: s.LastError,
	}
	if !s.LastRunAt.IsZero() {
		v.LastRunAt = s.LastRunAt.Format(time.RFC3339)
	}
	return v
}

func (s *Server) handleScheduleGet(w http.ResponseWriter, r *http.Request) {
	o, ok := s.backupOwner(w, r)
	if !ok {
		return
	}
	sc, err := s.db.BackupScheduleFor(r.Context(), o.ID)
	if errors.Is(err, db.ErrNotFound) {
		// Not an error: an account without a schedule simply has none, and
		// the UI needs somewhere sensible to start from.
		writeJSON(w, http.StatusOK, map[string]any{"schedule": nil, "owner": o.Username})
		return
	}
	if err != nil {
		s.log.Error("httpapi: read schedule", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedule": viewSchedule(sc)})
}

func (s *Server) handleScheduleSave(w http.ResponseWriter, r *http.Request) {
	o, ok := s.backupOwner(w, r)
	if !ok {
		return
	}
	var req struct {
		Enabled          bool   `json:"enabled"`
		Frequency        string `json:"frequency"`
		Hour             int    `json:"hour"`
		Weekday          int    `json:"weekday"`
		Keep             int    `json:"keep"`
		IncludeFiles     bool   `json:"include_files"`
		IncludeDatabases bool   `json:"include_databases"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sc := &db.BackupSchedule{
		OwnerID: o.ID, Enabled: req.Enabled, Frequency: req.Frequency,
		Hour: req.Hour, Weekday: req.Weekday, Keep: req.Keep,
		IncludeFiles: req.IncludeFiles, IncludeDatabases: req.IncludeDatabases,
	}
	if err := backups.ValidateSchedule(sc); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.db.SaveBackupSchedule(r.Context(), sc); err != nil {
		s.log.Error("httpapi: save schedule", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "backup.schedule", o.Username, true,
		fmt.Sprintf("%s at %02d:00, keep %d", sc.Frequency, sc.Hour, sc.Keep))

	saved, err := s.db.BackupScheduleFor(r.Context(), o.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedule": viewSchedule(saved)})
}

func (s *Server) handleScheduleDelete(w http.ResponseWriter, r *http.Request) {
	o, ok := s.backupOwner(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteBackupSchedule(r.Context(), o.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "backup.schedule.delete", o.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// trimAgent drops the "agent action x failed (code): " wrapper from a message
// on its way to a customer, who can do nothing with it.
func trimAgent(msg string) string {
	if i := strings.Index(msg, "): "); i > 0 && strings.HasPrefix(msg, "agent action ") {
		return msg[i+3:]
	}
	return msg
}
