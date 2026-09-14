package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/phpini"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
)

// handlePHPSettings returns the catalogue and one version's current values.
//
// Readable by any signed-in user: these are the limits their websites run
// under, and a customer asking "why did my import stop" deserves to be able
// to look rather than open a ticket. Changing them is administrator-only,
// because one change lands on every website using that version.
func (s *Server) handlePHPSettings(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")
	if !phpmgr.ValidVersion(version) {
		writeError(w, http.StatusBadRequest, "bad_request", "that is not a PHP version")
		return
	}
	values, err := s.db.PHPVersionSettings(r.Context(), version)
	if err != nil {
		s.log.Error("httpapi: read php settings", "version", version, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	// What this interpreter is actually configured with, so an empty field
	// shows the value in force on this host rather than a number compiled
	// into the panel.
	catalogue := phpini.Catalogue()
	live, liveErr := agentclient.Call[actions.PHPIniReadResult](r.Context(), s.agent,
		"php.ini.read", 1, actions.PHPVersionRequest{Version: version})
	if liveErr == nil {
		for i := range catalogue {
			if v, ok := live.Values[catalogue[i].Name]; ok && v != "" {
				catalogue[i].Default = v
			}
		}
		// The panel replaces its own drop-in wholesale when it saves. If
		// that file already carries settings the panel does not know about
		// -- the installer's tuning, most often -- an empty form would
		// quietly throw them away on the first save. Adopting them once
		// makes what is on screen what is on disk.
		if len(values) == 0 && len(live.Managed) > 0 {
			if adopted, err := phpini.Check(live.Managed); err == nil && len(adopted) > 0 {
				if err := s.db.ReplacePHPVersionSettings(r.Context(), version, adopted); err == nil {
					values = adopted
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":    version,
		"directives": catalogue,
		"values":     values,
	})
}

// handlePHPSettingsSave rewrites one version's ini drop-in.
//
// The whole set is submitted every time, so a directive left out goes back to
// PHP's own default. That makes "reset this" a matter of clearing a field
// rather than a second endpoint nobody would find.
func (s *Server) handlePHPSettingsSave(w http.ResponseWriter, r *http.Request) {
	version := chi.URLParam(r, "version")
	if !phpmgr.ValidVersion(version) {
		writeError(w, http.StatusBadRequest, "bad_request", "that is not a PHP version")
		return
	}
	var req struct {
		Values map[string]string `json:"values"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	checked, err := phpini.Check(req.Values)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_php_setting", err.Error())
		return
	}

	// The file is written first and the database second. The other order
	// would leave the panel claiming a setting the interpreter never got, on
	// a host where the write failed.
	res, err := agentclient.Call[actions.PHPIniResult](r.Context(), s.agent, "php.ini.write", 1,
		actions.PHPIniRequest{Version: version, Settings: checked})
	if err != nil {
		s.audit(r, "php.settings", version, false, err.Error())
		writeError(w, http.StatusBadRequest, "write_failed", trimAgent(err.Error()))
		return
	}
	if err := s.db.ReplacePHPVersionSettings(r.Context(), version, checked); err != nil {
		s.log.Error("httpapi: save php settings", "version", version, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "php.settings", version, true, res.Path)

	out := map[string]any{"version": version, "values": checked, "path": res.Path}
	if !res.Reloaded {
		// The file is correct and the running interpreters are not. Saying so
		// is the difference between "it did not work" and "restart the
		// webserver".
		out["warning"] = "Saved, but the webserver did not reload. " +
			"The new settings apply once it restarts."
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePHPOverrides lists websites that set php.ini values of their own.
//
// The version settings page is the one place these are decided, so it has to
// admit when a website is not following them. Without this, an operator
// raises a limit, one site does not change, and there is no screen that
// explains why.
func (s *Server) handlePHPOverrides(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.SitesWithPHPOverrides(r.Context())
	if err != nil {
		s.log.Error("httpapi: list php overrides", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	actor := userFrom(r.Context())
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// Scoped like every other list: a reseller sees their subtree, a
		// customer sees their own.
		if !auth.OwnsResource(actor, row.OwnerID, 0) {
			continue
		}
		out = append(out, map[string]any{
			"site_id": row.SiteID,
			"domain":  row.Domain,
			"php":     row.PHPVersion,
			"values":  row.Values,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": out})
}

// handlePHPOverrideClear drops one website's overrides, putting it back on
// its version's settings.
func (s *Server) handlePHPOverrideClear(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	if err := s.db.ReplaceSitePHPSettings(r.Context(), site.ID, nil); err != nil {
		s.log.Error("httpapi: clear php overrides", "site", site.Domain, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.sites.SyncWebserver(r.Context()); err != nil {
		s.audit(r, "site.php_settings", site.Domain, false, err.Error())
		s.siteError(w, err)
		return
	}
	s.audit(r, "site.php_settings", site.Domain, true, "overrides cleared")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "domain": site.Domain})
}
