package httpapi

import (
	"net/http"
	"strings"
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
	if err := s.files.Archive(r.Context(), o, req.Paths, req.Dest); err != nil {
		s.fileError(w, "archive", err)
		return
	}
	s.audit(r, "file.archive", o.Username+":"+req.Dest, true,
		strings.Join(req.Paths, ", "))
	writeJSON(w, http.StatusOK, map[string]any{"archive": req.Dest})
}

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
	res, err := s.files.Extract(r.Context(), o, req.Path, req.Dest)
	if err != nil {
		s.fileError(w, "extract", err)
		return
	}
	s.audit(r, "file.extract", o.Username+":"+req.Path, true, res.Dest)
	writeJSON(w, http.StatusOK, res)
}
