package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// logRequest builds an agent request from the query string, having first
// checked that the caller owns the site.
func (s *Server) logRequest(w http.ResponseWriter, r *http.Request) (actions.LogTailRequest, bool) {
	id, err := strconv.ParseInt(r.URL.Query().Get("site"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "which website?")
		return actions.LogTailRequest{}, false
	}
	site, err := s.db.SiteByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such website")
		return actions.LogTailRequest{}, false
	}
	// Ownership decided the same way as everywhere else; a log holds
	// visitors' addresses and request paths, so it is not public within the
	// server.
	if !s.ownsSite(r, site) {
		writeError(w, http.StatusNotFound, "not_found", "no such website")
		return actions.LogTailRequest{}, false
	}

	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = actions.LogAccess
	}
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	return actions.LogTailRequest{
		Owner:  site.OwnerUsername,
		Domain: site.Domain,
		Kind:   kind,
		Lines:  lines,
		Filter: r.URL.Query().Get("q"),
	}, true
}

func (s *Server) handleLogTail(w http.ResponseWriter, r *http.Request) {
	req, ok := s.logRequest(w, r)
	if !ok {
		return
	}
	res, err := agentclient.Call[actions.LogTailResult](r.Context(), s.agent, "log.tail", 1, req)
	if err != nil {
		s.fileError(w, "log", err)
		return
	}
	if res.Lines == nil {
		res.Lines = []string{}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleLogDownload(w http.ResponseWriter, r *http.Request) {
	req, ok := s.logRequest(w, r)
	if !ok {
		return
	}
	staged, err := agentclient.Call[actions.FileStageResult](r.Context(), s.agent, "log.stage", 1, req)
	if err != nil {
		s.fileError(w, "log", err)
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

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.FormatInt(staged.Size, 10))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", req.Domain+"-"+req.Kind+".log"))
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("httpapi: log download interrupted", "domain", req.Domain, "err", err)
	}
}

// ownsSite is the shared ownership test for anything hanging off a website.
func (s *Server) ownsSite(r *http.Request, site *db.Site) bool {
	return auth.OwnsResource(userFrom(r.Context()), site.OwnerID, site.OwnerParentID)
}
