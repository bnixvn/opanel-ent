package httpapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/filemanager"
)

// MaxUploadBytes caps a single uploaded file. Generous enough for a plugin
// archive or a database dump, small enough that a customer cannot fill the
// panel's state directory before the agent moves the file into their own
// home, where their disk quota applies.
const MaxUploadBytes = 256 << 20 // 256 MiB

// owner resolves the account whose files this request is about, writing the
// error response itself and returning false when the caller must stop.
func (s *Server) owner(w http.ResponseWriter, r *http.Request) (filemanager.Owner, bool) {
	o, err := s.files.ResolveOwner(r.Context(), userFrom(r.Context()), r.URL.Query().Get("owner"))
	switch {
	case errors.Is(err, filemanager.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "you may only browse your own files")
		return filemanager.Owner{}, false
	case errors.Is(err, filemanager.ErrNoAccount):
		writeError(w, http.StatusBadRequest, "no_account",
			"this account has no home directory; pick a hosting account with ?owner=")
		return filemanager.Owner{}, false
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return filemanager.Owner{}, false
	}
	return o, true
}

// fileError turns an agent failure into a status code. A path the customer
// mistyped and a bug in the panel deserve different answers, and the agent
// reports the underlying errno text, so match on that.
func (s *Server) fileError(w http.ResponseWriter, op string, err error) {
	// Only a reply from the agent describes the customer's file. Anything
	// else is the socket, and its "no such file or directory" is about the
	// socket itself -- reporting that as a missing file would send someone
	// hunting for a path that is perfectly fine.
	var agentErr *agentclient.Error
	if !errors.As(err, &agentErr) {
		s.log.Error("httpapi: file operation could not reach the agent", "op", op, "err", err)
		writeError(w, http.StatusInternalServerError, "agent_unavailable",
			"the management agent is not reachable")
		return
	}
	msg := agentErr.Msg
	switch {
	// A rejected payload is the customer's mistake by definition: the agent
	// validated the request and said no before touching anything.
	case agentErr.Code == agent.CodeBadPayload:
		writeError(w, http.StatusBadRequest, "bad_request",
			strings.TrimPrefix(msg, "invalid payload: "))
	case agentErr.Code == agent.CodeDenied:
		writeError(w, http.StatusForbidden, "denied", msg)
	case strings.Contains(msg, "no such file or directory"):
		writeError(w, http.StatusNotFound, "not_found", "no such file or directory")
	case strings.Contains(msg, "file exists"):
		writeError(w, http.StatusConflict, "exists", "a file of that name is already there")
	case strings.Contains(msg, "directory not empty"):
		writeError(w, http.StatusConflict, "not_empty", "the directory is not empty")
	case strings.Contains(msg, "not a directory"), strings.Contains(msg, "is a directory"),
		strings.Contains(msg, "escapes from parent"), strings.Contains(msg, "path escapes"),
		strings.Contains(msg, "invalid argument"), strings.Contains(msg, "may not contain"),
		strings.Contains(msg, "is larger than"), strings.Contains(msg, "required"),
		// An archive that fails these checks is the customer's file, not a
		// fault in the panel, and reporting it as a 500 sends somebody
		// looking through the panel's logs for a problem that is in the zip.
		strings.Contains(msg, "already exists"),
		strings.Contains(msg, "the archive"),
		strings.Contains(msg, "is not a readable zip"),
		strings.Contains(msg, "is not gzip"),
		strings.Contains(msg, "is not a .zip"):
		writeError(w, http.StatusBadRequest, "bad_request", msg)
	default:
		s.log.Error("httpapi: file operation failed", "op", op, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", msg)
	}
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	res, err := s.files.List(r.Context(), o, r.URL.Query().Get("path"))
	if err != nil {
		s.fileError(w, "list", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"owner": o.Username,
		"path":  res.Path,
		// The absolute path, so the interface can show where these files
		// actually are rather than only where they are relative to a home
		// the reader cannot see.
		"home":     o.Home,
		"absolute": path.Join(o.Home, res.Path),
		"entries":  res.Entries,
	})
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	res, err := s.files.Read(r.Context(), o, r.URL.Query().Get("path"))
	if err != nil {
		s.fileError(w, "read", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": res.Path, "content": res.Content,
		"size": res.Size, "truncated": res.Truncated,
	})
}

func (s *Server) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.files.Write(r.Context(), o, req.Path, req.Content); err != nil {
		s.fileError(w, "write", err)
		return
	}
	s.audit(r, "file.write", o.Username+":"+req.Path, true, strconv.Itoa(len(req.Content))+" bytes")
	writeJSON(w, http.StatusOK, map[string]any{"saved": true})
}

func (s *Server) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.files.Mkdir(r.Context(), o, req.Path); err != nil {
		s.fileError(w, "mkdir", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"created": req.Path})
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	if err := s.files.Delete(r.Context(), o, p); err != nil {
		s.fileError(w, "delete", err)
		return
	}
	s.audit(r, "file.delete", o.Username+":"+p, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": p})
}

func (s *Server) handleFileRename(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.files.Rename(r.Context(), o, req.From, req.To); err != nil {
		s.fileError(w, "rename", err)
		return
	}
	s.audit(r, "file.rename", o.Username+":"+req.From, true, "to "+req.To)
	writeJSON(w, http.StatusOK, map[string]any{"renamed": req.To})
}

func (s *Server) handleFileChmod(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.files.Chmod(r.Context(), o, req.Path, req.Mode); err != nil {
		s.fileError(w, "chmod", err)
		return
	}
	s.audit(r, "file.chmod", o.Username+":"+req.Path, true, "mode "+req.Mode)
	writeJSON(w, http.StatusOK, map[string]any{"mode": req.Mode})
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	f, err := s.files.Download(r.Context(), o, r.URL.Query().Get("path"))
	if err != nil {
		s.fileError(w, "download", err)
		return
	}
	defer func() { _ = f.Close() }()

	// The filename comes from the customer's own directory, so quote it and
	// strip anything that would let it break out of the header value.
	name := strings.NewReplacer("\"", "", "\r", "", "\n", "", ";", "").Replace(f.Name)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if _, err := io.Copy(w, f.File); err != nil {
		s.log.Warn("httpapi: download interrupted", "owner", o.Username, "err", err)
	}
}

func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	dir := r.URL.Query().Get("path")

	// Streamed rather than ParseMultipartForm, which buffers the whole body
	// in memory above its threshold and gives an attacker a cheap way to use
	// the panel's RAM.
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "expected a multipart upload")
		return
	}
	var saved []string
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
		// path.Base defeats a browser (or a script) that sends a filename
		// with directories in it; the destination directory is the one named
		// in the query, never one the file gets to choose.
		name := path.Base(part.FileName())
		if name == "." || name == "/" {
			_ = part.Close()
			writeError(w, http.StatusBadRequest, "bad_request", "the upload has no usable file name")
			return
		}
		dest := path.Join(dir, name)
		err = s.files.Upload(r.Context(), o, dest, part, MaxUploadBytes)
		_ = part.Close()
		if errors.Is(err, filemanager.ErrTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large",
				fmt.Sprintf("%s is larger than the %d MB upload limit; use SFTP for files this size",
					name, MaxUploadBytes>>20))
			return
		}
		if err != nil {
			s.fileError(w, "upload", err)
			return
		}
		saved = append(saved, dest)
	}
	if len(saved) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "no file was included")
		return
	}
	s.audit(r, "file.upload", o.Username+":"+strings.Join(saved, " "), true, "")
	writeJSON(w, http.StatusCreated, map[string]any{"uploaded": saved})
}

// handleFileLimits tells the browser what the server will accept, so the UI
// can refuse a too-large file before spending the upload.
func (s *Server) handleFileLimits(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"max_upload_bytes": int64(MaxUploadBytes),
		"max_edit_bytes":   int64(actions.MaxEditBytes),
	})
}
