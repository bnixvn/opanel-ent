package httpapi

import (
	"net/http"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/acme"
	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// handleCertList reports every certificate the panel holds.
//
// Admin only: the list spans every customer on the server, and one
// customer's domain names are not another's business.
func (s *Server) handleCertList(w http.ResponseWriter, r *http.Request) {
	res, err := agentclient.Call[actions.CertListResult](r.Context(), s.agent, "cert.list", 1, struct{}{})
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleCertReusable reports which stored certificate already covers a site,
// which is what makes adding a subdomain under an existing wildcard a
// one-click operation rather than a fresh order.
func (s *Server) handleCertReusable(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	found, err := agentclient.Call[*acme.Stored](r.Context(), s.agent, "cert.covering", 1,
		actions.CertCoverRequest{Host: site.Domain})
	if err != nil {
		s.agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"certificate": found})
}

// handleSiteCertificateWildcard orders a wildcard over DNS-01.
//
// The token is used and not stored. Keeping a Cloudflare key that can edit
// DNS for a whole zone, in order to save the customer retyping it once every
// ninety days, is a trade the panel should not make on their behalf --
// renewal falls back to HTTP-01 for the names it can validate that way.
func (s *Server) handleSiteCertificateWildcard(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	var req struct {
		Domain     string `json:"domain"`
		CFToken    string `json:"cf_token"`
		ForceHTTPS bool   `json:"force_https"`
		Staging    bool   `json:"staging"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	base := strings.TrimSpace(strings.ToLower(req.Domain))
	if base == "" {
		base = baseDomain(site.Domain)
	}

	cert, err := agentclient.Call[acme.Certificate](r.Context(), s.agent, "cert.wildcard", 1,
		actions.CertWildcardRequest{
			Domain: base, CFToken: req.CFToken,
			Email: s.cfg.ACMEEmail, Staging: req.Staging,
		})
	if err != nil {
		s.audit(r, "cert.wildcard", base, false, err.Error())
		writeError(w, http.StatusBadRequest, "wildcard_failed", trimAgent(err.Error()))
		return
	}
	s.audit(r, "cert.wildcard", base, true, "")

	updated, err := s.sites.AttachCertificate(r.Context(), site.ID,
		cert.CertFile, cert.KeyFile, cert.NotAfter, req.ForceHTTPS)
	if err != nil {
		s.siteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site": viewSite(updated), "certificate": cert,
	})
}

// handleSiteCertificateReuse points a site at a certificate the panel
// already holds.
func (s *Server) handleSiteCertificateReuse(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	var req struct {
		Name       string `json:"name"`
		ForceHTTPS bool   `json:"force_https"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	// Asking the agent which certificate covers this host, rather than
	// trusting the name the browser sent, is what stops one customer
	// attaching another customer's certificate -- and its private key --
	// to their own site.
	found, err := agentclient.Call[*acme.Stored](r.Context(), s.agent, "cert.covering", 1,
		actions.CertCoverRequest{Host: site.Domain})
	if err != nil {
		s.agentError(w, err)
		return
	}
	if found == nil {
		writeError(w, http.StatusBadRequest, "no_certificate",
			"no certificate on this server covers "+site.Domain)
		return
	}
	if req.Name != "" && req.Name != found.Name {
		writeError(w, http.StatusBadRequest, "no_certificate",
			"the certificate you named does not cover "+site.Domain)
		return
	}

	updated, err := s.sites.AttachCertificate(r.Context(), site.ID,
		found.CertFile, found.KeyFile, found.NotAfter, req.ForceHTTPS)
	if err != nil {
		s.siteError(w, err)
		return
	}
	s.audit(r, "cert.reuse", site.Domain, true, found.Name)
	writeJSON(w, http.StatusOK, map[string]any{
		"site": viewSite(updated), "certificate": found,
	})
}

// handleSiteCertificateManual installs a certificate bought elsewhere.
func (s *Server) handleSiteCertificateManual(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	var req struct {
		Certificate string `json:"certificate"`
		PrivateKey  string `json:"private_key"`
		CABundle    string `json:"ca_bundle"`
		ForceHTTPS  bool   `json:"force_https"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	st, err := agentclient.Call[*acme.Stored](r.Context(), s.agent, "cert.manual", 1,
		actions.CertManualRequest{
			Domain: site.Domain, Certificate: req.Certificate,
			PrivateKey: req.PrivateKey, CABundle: req.CABundle,
		})
	if err != nil {
		s.audit(r, "cert.manual", site.Domain, false, err.Error())
		writeError(w, http.StatusBadRequest, "certificate_rejected", trimAgent(err.Error()))
		return
	}
	s.audit(r, "cert.manual", site.Domain, true, st.Issuer)

	updated, err := s.sites.AttachCertificate(r.Context(), site.ID,
		st.CertFile, st.KeyFile, st.NotAfter, req.ForceHTTPS)
	if err != nil {
		s.siteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site": viewSite(updated), "certificate": st,
	})
}

// handleCertDelete removes a stored certificate. Admin only, and refused
// while a site is still using it: removing one out from under a live site
// stops the webserver reloading.
func (s *Server) handleCertDelete(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "which certificate?")
		return
	}
	sites, err := s.db.ListSites(r.Context(), db.ScopeAll())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	var inUse []string
	for _, site := range sites {
		if site.SSLEnabled && strings.Contains(site.CertFile, "/"+name+"/") {
			inUse = append(inUse, site.Domain)
		}
	}
	if len(inUse) > 0 {
		writeJSON(w, http.StatusConflict, errorBody{
			Error:   "that certificate is still used by " + strings.Join(inUse, ", "),
			Code:    "certificate_in_use",
			Details: map[string]any{"sites": inUse},
		})
		return
	}

	if _, err := agentclient.Call[struct{}](r.Context(), s.agent, "cert.delete", 1,
		actions.CertNameRequest{Name: name}); err != nil {
		s.agentError(w, err)
		return
	}
	s.audit(r, "cert.delete", name, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// baseDomain strips one label, turning shop.example.com into example.com so
// a wildcard for the parent zone is the obvious default.
func baseDomain(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	return strings.Join(parts[1:], ".")
}

// requireSiteOwner is the ownership test the SSL routes share.
func (s *Server) requireSiteOwner(w http.ResponseWriter, r *http.Request, site *db.Site) bool {
	if auth.OwnsResource(userFrom(r.Context()), site.OwnerID, site.OwnerParentID) {
		return true
	}
	writeError(w, http.StatusNotFound, "not_found", "no such website")
	return false
}
