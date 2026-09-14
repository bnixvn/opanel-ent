package httpapi

import (
	"net/http"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/wordpress"
)

// handleWordPressList returns every website the caller can see that has
// WordPress in it.
//
// The list is built from the sites table and checked against the disk,
// because the panel does not own what is in a document root: a customer can
// install WordPress over SFTP, or delete it, without telling the panel.
func (s *Server) handleWordPressList(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r.Context())
	rows, err := s.db.ListSites(r.Context(), auth.ScopeFor(actor))
	if err != nil {
		s.log.Error("httpapi: list sites", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, site := range rows {
		if site.AppType != "wordpress" && site.AppType != "php" {
			continue
		}
		st, err := s.wordpress.Status(r.Context(), site)
		if err != nil || !st.Installed {
			continue
		}
		out = append(out, map[string]any{
			"site_id":   site.ID,
			"domain":    site.Domain,
			"owner":     site.OwnerUsername,
			"php":       site.PHPVersion,
			"ssl":       site.SSLEnabled,
			"version":   st.Version,
			"suspended": site.Suspended,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

// handleWordPressInfo returns one installation in detail.
//
// Separate from the list because it costs several WP-CLI calls and one of
// them reaches wordpress.org: doing it for every site up front would make
// the page take as long as the slowest site on the host.
func (s *Server) handleWordPressInfo(w http.ResponseWriter, r *http.Request) {
	site := s.loadWordPressSite(w, r)
	if site == nil {
		return
	}
	info, err := s.wordpress.Info(r.Context(), site)
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"domain": site.Domain, "owner": site.OwnerUsername,
		"php": site.PHPVersion, "info": info,
	})
}

// handleWordPressManage updates or toggles core, a plugin or a theme.
func (s *Server) handleWordPressManage(w http.ResponseWriter, r *http.Request) {
	site := s.loadWordPressSite(w, r)
	if site == nil {
		return
	}
	var req struct {
		Kind string `json:"kind"`
		Op   string `json:"op"`
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.wordpress.Manage(r.Context(), site, req.Kind, req.Op, req.Name)
	target := site.Domain + ":" + req.Kind
	if req.Name != "" {
		target += "/" + req.Name
	}
	if err != nil {
		s.audit(r, "wordpress."+req.Op, target, false, err.Error())
		writeError(w, http.StatusBadRequest, "wp_failed", trimAgent(err.Error()))
		return
	}
	s.audit(r, "wordpress."+req.Op, target, true, "")
	writeJSON(w, http.StatusOK, res)
}

// handleWordPressSignOn hands back a one-click link into wp-admin.
//
// The panel has no copy of the customer's WordPress password -- resetting one
// to get in would lock them out of their own site -- so this mints a
// single-use token instead, good for a couple of minutes.
func (s *Server) handleWordPressSignOn(w http.ResponseWriter, r *http.Request) {
	site := s.loadWordPressSite(w, r)
	if site == nil {
		return
	}
	if site.Suspended {
		writeError(w, http.StatusConflict, "suspended",
			"this website is suspended, so there is nothing to sign in to")
		return
	}
	res, err := s.wordpress.SignOn(r.Context(), site)
	if err != nil {
		s.audit(r, "wordpress.signon", site.Domain, false, err.Error())
		writeError(w, http.StatusBadRequest, "wp_failed", trimAgent(err.Error()))
		return
	}
	s.audit(r, "wordpress.signon", site.Domain, true, res.User)
	writeJSON(w, http.StatusOK, res)
}

// loadWordPressSite resolves the site and checks the caller may reach it.
func (s *Server) loadWordPressSite(w http.ResponseWriter, r *http.Request) *db.Site {
	site := s.loadSite(w, r)
	if site == nil {
		return nil
	}
	if !wordpress.CanReach(userFrom(r.Context()), site) {
		writeError(w, http.StatusNotFound, "not_found", "no such website")
		return nil
	}
	return site
}
