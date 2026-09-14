package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/db"
)

type firewallRuleView struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Protocol  string `json:"protocol"`
	PortFrom  int    `json:"port_from"`
	PortTo    int    `json:"port_to"`
	Address   string `json:"address"`
	Comment   string `json:"comment"`
	Enabled   bool   `json:"enabled"`
	Managed   bool   `json:"managed"`
	Protected bool   `json:"protected"`
}

func viewFirewallRule(r *db.FirewallRule) firewallRuleView {
	v := firewallRuleView{
		ID: r.ID, Kind: r.Kind, Protocol: r.Protocol,
		PortFrom: r.PortFrom, PortTo: r.PortTo, Address: r.Address,
		Comment: r.Comment, Enabled: r.Enabled, Managed: r.Managed,
	}
	return v
}

func (s *Server) handleFirewallList(w http.ResponseWriter, r *http.Request) {
	rules, err := s.db.ListFirewallRules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	sources, err := s.db.ListFirewallSources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	out := make([]firewallRuleView, 0, len(rules))
	for _, rule := range rules {
		out = append(out, viewFirewallRule(rule))
	}
	srcOut := make([]map[string]any, 0, len(sources))
	for _, src := range sources {
		entry := map[string]any{
			"id": src.ID, "url": src.URL, "description": src.Description,
			"enabled": src.Enabled, "interval_hours": src.IntervalHours,
			"entry_count": src.EntryCount, "last_error": src.LastError,
		}
		if !src.LastFetchAt.IsZero() {
			entry["last_fetch_at"] = src.LastFetchAt
		}
		srcOut = append(srcOut, entry)
	}

	body := map[string]any{"rules": out, "sources": srcOut}
	if st, err := s.firewall.Status(r.Context()); err == nil {
		body["status"] = st
	} else {
		s.log.Warn("httpapi: firewall status", "err", err)
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleFirewallCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind     string `json:"kind"`
		Protocol string `json:"protocol"`
		PortFrom int    `json:"port_from"`
		PortTo   int    `json:"port_to"`
		Address  string `json:"address"`
		Comment  string `json:"comment"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Protocol == "" {
		req.Protocol = "tcp"
	}
	created, err := s.firewall.AddRule(r.Context(), &db.FirewallRule{
		Kind: req.Kind, Protocol: req.Protocol,
		PortFrom: req.PortFrom, PortTo: req.PortTo,
		Address: strings.TrimSpace(req.Address), Comment: req.Comment,
		Enabled: true,
	})
	if err != nil {
		s.audit(r, "firewall.rule.add", req.Address, false, err.Error())
		writeError(w, http.StatusBadRequest, "bad_request", trimAgent(err.Error()))
		return
	}
	s.audit(r, "firewall.rule.add", ruleLabel(created), true, "")
	writeJSON(w, http.StatusCreated, viewFirewallRule(created))
}

func (s *Server) handleFirewallDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "rule id must be a number")
		return
	}
	if err := s.firewall.DeleteRule(r.Context(), id); err != nil {
		s.audit(r, "firewall.rule.delete", chi.URLParam(r, "id"), false, err.Error())
		writeError(w, http.StatusBadRequest, "bad_request", trimAgent(err.Error()))
		return
	}
	s.audit(r, "firewall.rule.delete", chi.URLParam(r, "id"), true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFirewallConfirm keeps a change that is waiting to be rolled back.
//
// The browser calls this straight after a change. Reaching it is the proof
// that the new rules did not cut off the connection that made them; if they
// did, nothing arrives and the previous ruleset comes back on its own.
func (s *Server) handleFirewallConfirm(w http.ResponseWriter, r *http.Request) {
	if err := s.firewall.Confirm(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", trimAgent(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleFirewallSourceCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL           string `json:"url"`
		Description   string `json:"description"`
		IntervalHours int    `json:"interval_hours"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.IntervalHours <= 0 {
		req.IntervalHours = 24
	}
	src, err := s.db.CreateFirewallSource(r.Context(), &db.FirewallSource{
		URL: strings.TrimSpace(req.URL), Description: req.Description,
		Enabled: true, IntervalHours: req.IntervalHours,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Fetched at once so an operator finds out immediately that the URL is
	// wrong, rather than in a day when the timer first runs.
	count, ferr := s.firewall.RefreshSource(r.Context(), src.ID)
	if ferr != nil {
		s.audit(r, "firewall.source.add", src.URL, false, ferr.Error())
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": src.ID, "url": src.URL, "warning": trimAgent(ferr.Error()),
		})
		return
	}
	s.audit(r, "firewall.source.add", src.URL, true, strconv.Itoa(count)+" addresses")
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": src.ID, "url": src.URL, "entry_count": count,
	})
}

func (s *Server) handleFirewallSourceRefresh(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "source id must be a number")
		return
	}
	count, err := s.firewall.RefreshSource(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusBadRequest, "fetch_failed", trimAgent(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry_count": count})
}

func (s *Server) handleFirewallSourceDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "source id must be a number")
		return
	}
	if err := s.db.DeleteFirewallSource(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.firewall.Apply(r.Context(), true); err != nil {
		s.log.Warn("httpapi: reapply after removing a blocklist", "err", err)
	}
	s.audit(r, "firewall.source.delete", chi.URLParam(r, "id"), true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func ruleLabel(r *db.FirewallRule) string {
	if r.Kind == db.FirewallPort {
		return r.Protocol + "/" + strconv.Itoa(r.PortFrom)
	}
	return r.Kind + " " + r.Address
}

// --- WAF ---------------------------------------------------------------

func (s *Server) handleWAFStatus(w http.ResponseWriter, r *http.Request) {
	st, err := agentclient.Call[actions.WAFStatus](r.Context(), s.agent, "waf.status", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	sites, err := s.db.ListSites(r.Context(), db.ScopeAll())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	protected := make([]map[string]any, 0, len(sites))
	for _, site := range sites {
		protected = append(protected, map[string]any{
			"id": site.ID, "domain": site.Domain, "enabled": site.WAFEnabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"waf": st, "sites": protected})
}

func (s *Server) handleWAFInstall(w http.ResponseWriter, r *http.Request) {
	st, err := agentclient.Call[actions.WAFStatus](r.Context(), s.agent, "waf.install", 1, struct{}{})
	if err != nil {
		s.audit(r, "waf.install", "", false, err.Error())
		writeError(w, http.StatusBadRequest, "install_failed", trimAgent(err.Error()))
		return
	}
	s.audit(r, "waf.install", "", true, st.RulesVersion)
	writeJSON(w, http.StatusOK, map[string]any{"waf": st})
}

func (s *Server) handleWAFConfigure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode          string   `json:"mode"`
		ExcludedRules []string `json:"excluded_rules"`
		// The complete set of categories to leave out, every time: one left
		// out of this list is switched back on.
		DisabledFiles []string `json:"disabled_files"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	st, err := agentclient.Call[actions.WAFStatus](r.Context(), s.agent, "waf.configure", 1,
		actions.WAFConfigureRequest{
			Mode: req.Mode, ExcludedRules: req.ExcludedRules,
			DisabledFiles: req.DisabledFiles,
		})
	if err != nil {
		s.audit(r, "waf.configure", req.Mode, false, err.Error())
		writeError(w, http.StatusBadRequest, "bad_request", trimAgent(err.Error()))
		return
	}
	if err := s.sites.SyncWebserver(r.Context()); err != nil {
		s.siteError(w, err)
		return
	}
	s.audit(r, "waf.configure", req.Mode, true, "")
	writeJSON(w, http.StatusOK, map[string]any{"waf": st})
}

func (s *Server) handleWAFEvents(w http.ResponseWriter, r *http.Request) {
	res, err := agentclient.Call[actions.WAFEventsResult](r.Context(), s.agent, "waf.events", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	if res.Events == nil {
		res.Events = []actions.WAFEvent{}
	}
	writeJSON(w, http.StatusOK, res)
}

// handleSiteWAF turns the WAF on or off for one website.
func (s *Server) handleSiteWAF(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	site.WAFEnabled = req.Enabled
	if err := s.db.UpdateSite(r.Context(), site); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.sites.SyncWebserver(r.Context()); err != nil {
		s.siteError(w, err)
		return
	}
	s.audit(r, "waf.site", site.Domain, true, strconv.FormatBool(req.Enabled))
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": req.Enabled})
}

// handleWAFRules lists the rule set as categories somebody can switch off.
//
// Whole files rather than individual rule ids: the rules inside one are one
// kind of attack and are written to work together, and "turn off the SQL
// injection checks because this CMS stores SQL in a form field" is the
// decision an operator actually has to make after a false positive.
func (s *Server) handleWAFRules(w http.ResponseWriter, r *http.Request) {
	res, err := agentclient.Call[actions.WAFRulesResult](r.Context(), s.agent, "waf.rules", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
