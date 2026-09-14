package httpapi

import (
	"net/http"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/phpini"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// handleSitePHPSettings returns the catalogue and the site's current values.
//
// The catalogue travels with the values rather than sitting at its own
// endpoint, because the form cannot be drawn without both, and a directive
// the caller may not set has to be marked as such per caller.
func (s *Server) handleSitePHPSettings(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	values, err := s.db.SitePHPSettings(r.Context(), site.ID)
	if err != nil {
		s.log.Error("httpapi: read php settings", "site", site.Domain, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	actor := userFrom(r.Context())
	staff := auth.Role(actor.Role).AtLeast(auth.RoleReseller)
	catalogue := phpini.Catalogue()
	shown := make([]phpini.Directive, 0, len(catalogue))
	for _, d := range catalogue {
		if d.StaffOnly && !staff {
			continue
		}
		shown = append(shown, d)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"site":       site.Domain,
		"php":        site.PHPVersion,
		"runs_php":   site.AppType == webserver.AppPHP || site.AppType == webserver.AppWordPress,
		"directives": shown,
		"values":     values,
	})
}

// handleSitePHPSettingsSave replaces the site's overrides.
//
// The whole set is submitted every time, so a directive left out is a
// directive returned to PHP's default. That is what makes "reset" a matter of
// clearing a field rather than a second endpoint.
func (s *Server) handleSitePHPSettingsSave(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	if site.AppType != webserver.AppPHP && site.AppType != webserver.AppWordPress {
		writeError(w, http.StatusBadRequest, "not_php",
			"this website does not run PHP, so it has no PHP settings")
		return
	}

	var req struct {
		Values map[string]string `json:"values"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	actor := userFrom(r.Context())
	if !auth.Role(actor.Role).AtLeast(auth.RoleReseller) {
		// A customer submitting the whole form would otherwise clear the
		// staff-only directives simply by not having them on their page.
		current, err := s.db.SitePHPSettings(r.Context(), site.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		for _, name := range phpini.StaffOnlyNames() {
			if _, sent := req.Values[name]; sent {
				writeError(w, http.StatusForbidden, "forbidden",
					"\""+name+"\" can only be changed by an administrator")
				return
			}
			if v, ok := current[name]; ok {
				req.Values[name] = v
			}
		}
	}

	checked, err := phpini.Check(req.Values)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_php_setting", err.Error())
		return
	}
	if err := s.db.ReplaceSitePHPSettings(r.Context(), site.ID, checked); err != nil {
		s.log.Error("httpapi: save php settings", "site", site.Domain, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	// The settings live in the vhost, so they only take effect once the
	// configuration is rewritten and the server reloads.
	if err := s.sites.SyncWebserver(r.Context()); err != nil {
		s.audit(r, "site.php_settings", site.Domain, false, err.Error())
		s.siteError(w, err)
		return
	}
	s.audit(r, "site.php_settings", site.Domain, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "values": checked})
}
