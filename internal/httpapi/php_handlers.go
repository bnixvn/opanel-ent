package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
)

// handlePHPList reports every PHP version the active provider offers and
// which of them are installed. Readable by any authenticated user, because a
// site owner needs it to choose a version.
func (s *Server) handlePHPList(w http.ResponseWriter, r *http.Request) {
	res, err := agentclient.Call[actions.PHPListResult](r.Context(), s.agent, "php.list", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handlePHPInstall adds a version. Admin only: it changes the host.
func (s *Server) handlePHPInstall(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")
	if _, err := agentclient.Call[struct{}](r.Context(), s.agent, "php.install", 1,
		actions.PHPVersionRequest{Version: version}); err != nil {
		s.audit(r, "php.install", version, false, err.Error())
		s.agentError(w, err)
		return
	}
	s.audit(r, "php.install", version, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
}

// handlePHPUninstall removes a version.
func (s *Server) handlePHPUninstall(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")

	// Refuse while a site still points at it: removing the interpreter would
	// turn those sites into 503s with nothing in the panel explaining why.
	rows, err := s.db.ListSites(r.Context(), 0)
	if err != nil {
		s.log.Error("httpapi: list sites", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	var inUse []string
	for _, row := range rows {
		if row.PHPVersion == version {
			inUse = append(inUse, row.Domain)
		}
	}
	if len(inUse) > 0 {
		writeJSON(w, http.StatusConflict, errorBody{
			Error:   "PHP " + version + " is still used by " + itoa(len(inUse)) + " site(s)",
			Code:    "php_in_use",
			Details: map[string]any{"sites": inUse},
		})
		return
	}

	if _, err := agentclient.Call[struct{}](r.Context(), s.agent, "php.uninstall", 1,
		actions.PHPVersionRequest{Version: version}); err != nil {
		s.audit(r, "php.uninstall", version, false, err.Error())
		s.agentError(w, err)
		return
	}
	s.audit(r, "php.uninstall", version, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
}

// handleWebserverStatus reports which backend is serving.
func (s *Server) handleWebserverStatus(w http.ResponseWriter, r *http.Request) {
	res, err := agentclient.Call[actions.WebserverStatusResult](r.Context(), s.agent, "webserver.status", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleWebserverSync re-renders the configuration from the database. It is
// the repair button for a host whose config was edited by hand.
func (s *Server) handleWebserverSync(w http.ResponseWriter, r *http.Request) {
	if err := s.sites.SyncWebserver(r.Context()); err != nil {
		s.audit(r, "webserver.sync", "", false, err.Error())
		s.siteError(w, err)
		return
	}
	s.audit(r, "webserver.sync", "", true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
