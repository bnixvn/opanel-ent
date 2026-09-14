package httpapi

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/wordpress"
)

func (s *Server) handleWordPressStatus(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	st, err := s.wordpress.Status(r.Context(), site)
	if err != nil {
		s.log.Error("httpapi: wordpress status", "site", site.Domain, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", trimAgent(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"wordpress": st})
}

// titleFallback keeps a generated title readable when the customer leaves the
// field empty: the domain is a better default than "My Site".
func titleFallback(domain string) string { return domain }

// adminUserFallback builds a WordPress administrator name from the panel
// account. Not "admin": every bot on the internet tries that name first.
var nonAdminChar = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func adminUserFallback(owner string) string {
	name := nonAdminChar.ReplaceAllString(owner, "")
	if len(name) < 3 {
		name = "wpadmin"
	}
	return name
}

func (s *Server) handleWordPressInstall(w http.ResponseWriter, r *http.Request) {
	site := s.loadSite(w, r)
	if site == nil {
		return
	}
	var req struct {
		Title      string `json:"title"`
		AdminUser  string `json:"admin_user"`
		AdminEmail string `json:"admin_email"`
		Locale     string `json:"locale"`
		UseHTTPS   *bool  `json:"use_https"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}

	if strings.TrimSpace(req.Title) == "" {
		req.Title = titleFallback(site.Domain)
	}
	if strings.TrimSpace(req.AdminUser) == "" {
		req.AdminUser = adminUserFallback(site.OwnerUsername)
	}
	if strings.TrimSpace(req.AdminEmail) == "" {
		// The website owner's address, then whoever is installing it. A
		// password reset for the customer's own WordPress should reach the
		// customer, not the member of staff who happened to click the button.
		req.AdminEmail = s.acmeContact(r, site.OwnerID)
	}
	if req.AdminEmail == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"an administrator email address is required; WordPress uses it for "+
				"password resets. Set one on the owner's account, or on your own.")
		return
	}

	// HTTPS unless the caller says otherwise, and only when the site has a
	// certificate: WordPress writes the URL into its database and then
	// refuses to serve on any other scheme, so promising https on a site
	// that cannot answer on it produces a redirect loop, not a warning.
	useHTTPS := site.SSLEnabled
	if req.UseHTTPS != nil {
		useHTTPS = *req.UseHTTPS
	}
	if useHTTPS && !site.SSLEnabled {
		writeError(w, http.StatusBadRequest, "no_certificate",
			"this site has no certificate yet; turn SSL on first, or install over http")
		return
	}

	res, err := s.wordpress.Install(r.Context(), wordpress.Request{
		Site: site, Title: req.Title, AdminUser: req.AdminUser,
		AdminEmail: req.AdminEmail, Locale: req.Locale, UseHTTPS: useHTTPS,
	})
	if err != nil {
		s.audit(r, "wordpress.install", site.Domain, false, err.Error())
		writeError(w, http.StatusBadRequest, "install_failed", trimAgent(err.Error()))
		return
	}
	s.audit(r, "wordpress.install", site.Domain, true, "version "+res.Version)

	// The administrator password is shown once and never stored in a form
	// the panel can read back, which is the same contract as a new database
	// account's password.
	writeJSON(w, http.StatusCreated, map[string]any{"wordpress": res})
}
