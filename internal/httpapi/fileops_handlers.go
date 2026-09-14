package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/db"
)

func (s *Server) handleFileCopy(w http.ResponseWriter, r *http.Request) {
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
	if err := s.files.Copy(r.Context(), o, req.From, req.To); err != nil {
		s.fileError(w, "copy", err)
		return
	}
	s.audit(r, "file.copy", o.Username+":"+req.From, true, "to "+req.To)
	writeJSON(w, http.StatusOK, map[string]any{"copied": req.To})
}

// handleFileChmodRecursive applies a mode to a whole tree.
//
// Separate from chmod rather than a flag on it, because a mistake here is
// not recoverable by repeating the call: once every directory in a tree has
// lost its execute bit, the customer cannot list it to put it back.
func (s *Server) handleFileChmodRecursive(w http.ResponseWriter, r *http.Request) {
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
	if err := s.files.ChmodRecursive(r.Context(), o, req.Path, req.Mode); err != nil {
		s.fileError(w, "chmod", err)
		return
	}
	s.audit(r, "file.chmod_recursive", o.Username+":"+req.Path, true, "mode "+req.Mode)
	writeJSON(w, http.StatusOK, map[string]any{"mode": req.Mode})
}

// handleFileArchive packs paths into an archive, in the background.
//
// Answering with a job rather than the finished archive is the point: packing
// a few gigabytes takes minutes, and on the request's own context the work
// stopped the moment the tab closed or the phone changed network.
func (s *Server) handleFileArchive(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	var req struct {
		Paths []string `json:"paths"`
		Dest  string   `json:"dest"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s.audit(r, "file.archive", o.Username+":"+req.Dest, true,
		strings.Join(req.Paths, ", "))

	job, err := s.startFileJob(r, o, db.FileJobArchive, req.Dest,
		func(ctx context.Context) (string, error) {
			if err := s.files.Archive(ctx, o, req.Paths, req.Dest); err != nil {
				return "", err
			}
			return "packed " + describePaths(req.Paths), nil
		})
	if err != nil {
		s.log.Error("httpapi: start archive job", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": viewFileJob(job)})
}

// handleFileExtract unpacks an archive, in the background, for the same
// reason as handleFileArchive.
func (s *Server) handleFileExtract(w http.ResponseWriter, r *http.Request) {
	o, ok := s.owner(w, r)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
		Dest string `json:"dest"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s.audit(r, "file.extract", o.Username+":"+req.Path, true, req.Dest)

	job, err := s.startFileJob(r, o, db.FileJobExtract, req.Path,
		func(ctx context.Context) (string, error) {
			res, err := s.files.Extract(ctx, o, req.Path, req.Dest)
			if err != nil {
				return "", err
			}
			return trimDetail("unpacked into " + res.Dest), nil
		})
	if err != nil {
		s.log.Error("httpapi: start extract job", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": viewFileJob(job)})
}
