package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/sites"
)

type siteView struct {
	ID           int64     `json:"id"`
	Domain       string    `json:"domain"`
	Aliases      []string  `json:"aliases"`
	Owner        string    `json:"owner"`
	OwnerID      int64     `json:"owner_id"`
	AppType      string    `json:"app_type"`
	PHPVersion   string    `json:"php_version,omitempty"`
	DocumentRoot string    `json:"document_root"`
	RewriteMode  string    `json:"rewrite_mode"`
	SSLEnabled   bool      `json:"ssl_enabled"`
	ForceHTTPS   bool      `json:"force_https"`
	CertExpires  string    `json:"cert_expires,omitempty"`
	WAFEnabled   bool      `json:"waf_enabled"`
	Suspended    bool      `json:"suspended"`
	CreatedAt    time.Time `json:"created_at"`
}

func viewSite(s *db.Site) siteView {
	aliases := s.Aliases
	if aliases == nil {
		aliases = []string{} // an empty list, not null, so clients can iterate
	}
	return siteView{
		ID: s.ID, Domain: s.Domain, Aliases: aliases,
		Owner: s.OwnerUsername, OwnerID: s.OwnerID,
		AppType: s.AppType, PHPVersion: s.PHPVersion,
		DocumentRoot: s.DocumentRoot, RewriteMode: s.RewriteMode,
		SSLEnabled: s.SSLEnabled, ForceHTTPS: s.ForceHTTPS,
		CertExpires: certExpiresText(s), WAFEnabled: s.WAFEnabled,
		Suspended: s.Suspended, CreatedAt: s.CreatedAt,
	}
}

// certExpiresText renders the expiry for display, empty when there is none.
func certExpiresText(s *db.Site) string {
	if s.CertExpires.IsZero() {
		return ""
	}
	return s.CertExpires.Format(time.RFC3339)
}

func (s *Server) handleSiteList(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	rows, err := s.db.ListSites(r.Context(), sites.VisibleTo(u))
	if err != nil {
		s.log.Error("httpapi: list sites", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]siteView, 0, len(rows))
	for _, row := range rows {
		out = append(out, viewSite(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": out})
}

// loadSite resolves the :id parameter and enforces ownership. It writes the
// error response itself and returns nil when the caller must stop.
func (s *Server) loadSite(w http.ResponseWriter, r *http.Request) *db.Site {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "site id must be a number")
		return nil
	}
	site, err := s.db.SiteByID(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such site")
		return nil
	}
	if err != nil {
		s.log.Error("httpapi: load site", "err", err, "site_id", id)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return nil
	}
	u := userFrom(r.Context())
	// An end user asking for someone else's site gets 404, not 403: a 403
	// would confirm the site exists.
	if !auth.Role(u.Role).AtLeast(auth.RoleReseller) && site.OwnerID != u.ID {
		writeError(w, http.StatusNotFound, "not_found", "no such site")
		return nil
	}
	return site
}

func (s *Server) handleSiteGet(w http.ResponseWriter, r *http.Request) {
	if site := s.loadSite(w, r); site != nil {
		writeJSON(w, http.StatusOK, viewSite(site))
	}
}

type createSiteRequest struct {
	Domain      string   `json:"domain"`
	Aliases     []string `json:"aliases,omitempty"`
	OwnerID     int64    `json:"owner_id,omitempty"`
	AppType     string   `json:"app_type"`
	PHPVersion  string   `json:"php_version,omitempty"`
	RewriteMode string   `json:"rewrite_mode,omitempty"`
}

func (s *Server) handleSiteCreate(w http.ResponseWriter, r *http.Request) {
	var req createSiteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u := userFrom(r.Context())

	// Only an admin or reseller may create a site for someone else; everyone
	// else gets their own id regardless of what they sent.
	ownerID := req.OwnerID
	if !auth.Role(u.Role).AtLeast(auth.RoleReseller) || ownerID == 0 {
		ownerID = u.ID
	}

	site, err := s.sites.Create(r.Context(), sites.CreateRequest{
		Domain:      req.Domain,
		Aliases:     req.Aliases,
		OwnerID:     ownerID,
		AppType:     req.AppType,
		PHPVersion:  req.PHPVersion,
		RewriteMode: req.RewriteMode,
	})
	if err != nil {
		s.audit(r, "site.create", req.Domain, false, err.Error())
		s.siteError(w, err)
		return
	}
	s.audit(r, "site.create", site.Domain, true, "")
	writeJSON(w, http.StatusCreated, viewSite(site))
}

type updateSiteRequest struct {
	AppType     *string   `json:"app_type,omitempty"`
	PHPVersion  *string   `json:"php_version,omitempty"`
	Aliases     *[]string `json:"aliases,omitempty"`
	RewriteMode *string   `json:"rewrite_mode,omitempty"`
	Suspended   *bool     `json:"suspended,omitempty"`
	WAFEnabled  *bool     `json:"waf_enabled,omitempty"`
}

func (s *Server) handleSiteUpdate(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	var req updateSiteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Suspension is an administrative act; an owner must not lift their own.
	u := userFrom(r.Context())
	if req.Suspended != nil && !auth.Role(u.Role).AtLeast(auth.RoleReseller) {
		writeError(w, http.StatusForbidden, "forbidden", "only an administrator may suspend or unsuspend a site")
		return
	}

	updated, err := s.sites.Update(r.Context(), site.ID, sites.UpdateRequest{
		AppType:     req.AppType,
		PHPVersion:  req.PHPVersion,
		Aliases:     req.Aliases,
		RewriteMode: req.RewriteMode,
		Suspended:   req.Suspended,
		WAFEnabled:  req.WAFEnabled,
	})
	if err != nil {
		s.audit(r, "site.update", site.Domain, false, err.Error())
		s.siteError(w, err)
		return
	}
	s.audit(r, "site.update", site.Domain, true, "")
	writeJSON(w, http.StatusOK, viewSite(updated))
}

func (s *Server) handleSiteDelete(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	// Deleting files is opt-in through an explicit query parameter, so a
	// mistyped request removes a configuration and not a customer's data.
	removeFiles := r.URL.Query().Get("remove_files") == "true"

	if err := s.sites.Delete(r.Context(), site.ID, removeFiles); err != nil {
		s.audit(r, "site.delete", site.Domain, false, err.Error())
		s.siteError(w, err)
		return
	}
	s.audit(r, "site.delete", site.Domain, true,
		map[bool]string{true: "files removed", false: "files kept"}[removeFiles])
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// siteError maps service errors onto status codes.
func (s *Server) siteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sites.ErrDomainTaken):
		writeError(w, http.StatusConflict, "domain_taken", err.Error())
	case errors.Is(err, sites.ErrOwnerNotFound):
		writeError(w, http.StatusBadRequest, "bad_request", "no such owner")
	case errors.Is(err, sites.ErrPHPNotReady):
		writeError(w, http.StatusBadRequest, "php_not_installed", err.Error())
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such site")
	default:
		var ae *agentclient.Error
		if errors.As(err, &ae) {
			s.agentError(w, err)
			return
		}
		// Validation failures reach here as plain errors; they are the
		// caller's fault far more often than ours, and the message names the
		// field that is wrong.
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}

type certRequest struct {
	Email          string `json:"email,omitempty"`
	IncludeAliases bool   `json:"include_aliases,omitempty"`
	ForceHTTPS     bool   `json:"force_https,omitempty"`
	Staging        bool   `json:"staging,omitempty"`
}

// handleSiteCertificate obtains a Let's Encrypt certificate for a site.
//
// The request can take a while: the CA has to fetch the challenge over the
// public internet, and a domain whose DNS is wrong fails only after a
// timeout. The client is expected to wait rather than poll, because a
// half-finished order is not something the panel can resume.
func (s *Server) handleSiteCertificate(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	var req certRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	updated, err := s.sites.IssueCertificate(r.Context(), sites.CertificateRequest{
		SiteID:         site.ID,
		Email:          req.Email,
		IncludeAliases: req.IncludeAliases,
		ForceHTTPS:     req.ForceHTTPS,
		Staging:        req.Staging,
	})
	if err != nil {
		s.audit(r, "site.certificate", site.Domain, false, err.Error())
		s.siteError(w, err)
		return
	}
	s.audit(r, "site.certificate", site.Domain, true,
		"expires "+updated.CertExpires.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, viewSite(updated))
}

func (s *Server) handleSiteCertificateDelete(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	updated, err := s.sites.DisableTLS(r.Context(), site.ID)
	if err != nil {
		s.audit(r, "site.certificate.disable", site.Domain, false, err.Error())
		s.siteError(w, err)
		return
	}
	s.audit(r, "site.certificate.disable", site.Domain, true, "")
	writeJSON(w, http.StatusOK, viewSite(updated))
}
