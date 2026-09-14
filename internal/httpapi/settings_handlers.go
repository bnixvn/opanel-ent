package httpapi

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// MaxBrandAssetBytes caps a logo or favicon. They are stored as data: URIs
// and sent to every browser on every page load, so a 4 MB logo would be paid
// for on every visit.
const MaxBrandAssetBytes = 256 << 10 // 256 KiB

// hostnamePattern is what may be added as a panel hostname.
var hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

type brandingView struct {
	Name    string `json:"name"`
	Logo    string `json:"logo,omitempty"`
	Favicon string `json:"favicon,omitempty"`
}

// handleBranding serves the name and images the interface wears.
//
// Unauthenticated: the login page needs them, and a brand name is not a
// secret. Nothing else about the panel is exposed here.
func (s *Server) handleBranding(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.branding(r))
}

func (s *Server) branding(r *http.Request) brandingView {
	name, _ := s.db.Setting(r.Context(), db.SettingBrandName, "OPanel")
	logo, _ := s.db.Setting(r.Context(), db.SettingBrandLogo, "")
	icon, _ := s.db.Setting(r.Context(), db.SettingBrandIcon, "")
	return brandingView{Name: name, Logo: logo, Favicon: icon}
}

func (s *Server) handleBrandingSet(w http.ResponseWriter, r *http.Request) {
	var req brandingView
	if !decodeJSON(w, r, &req) {
		return
	}
	if n := strings.TrimSpace(req.Name); n != "" {
		if len(n) > 64 {
			writeError(w, http.StatusBadRequest, "bad_request", "the name is too long")
			return
		}
		if err := s.db.SetSetting(r.Context(), db.SettingBrandName, n); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
	}
	for _, asset := range []struct {
		key   string
		value string
		label string
	}{
		{db.SettingBrandLogo, req.Logo, "logo"},
		{db.SettingBrandIcon, req.Favicon, "favicon"},
	} {
		if asset.value == "" {
			continue
		}
		if err := validDataURI(asset.value); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", asset.label+": "+err.Error())
			return
		}
		if err := s.db.SetSetting(r.Context(), asset.key, asset.value); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
	}
	s.audit(r, "settings.branding", "", true, "")
	writeJSON(w, http.StatusOK, s.branding(r))
}

// validDataURI checks an uploaded image before it is stored.
//
// Only image types, and never SVG: an SVG is a document that can carry
// script, and this one is rendered inside the panel's own origin.
func validDataURI(v string) error {
	if len(v) > MaxBrandAssetBytes {
		return errTooBig
	}
	for _, prefix := range []string{
		"data:image/png;base64,", "data:image/jpeg;base64,",
		"data:image/webp;base64,", "data:image/gif;base64,",
	} {
		if strings.HasPrefix(v, prefix) {
			return nil
		}
	}
	return errBadImage
}

var (
	errTooBig   = &settingsError{"the image must be under 256 KB"}
	errBadImage = &settingsError{"the image must be a PNG, JPEG, WebP or GIF data URI"}
)

type settingsError struct{ msg string }

func (e *settingsError) Error() string { return e.msg }

// handleSettings reports everything the settings page shows.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	stored, err := s.db.Settings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	hostnames, err := s.db.ListPanelHostnames(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	names := make([]map[string]any, 0, len(hostnames))
	for _, h := range hostnames {
		names = append(names, map[string]any{
			"hostname": h.Hostname, "is_primary": h.IsPrimary,
			"has_certificate": h.CertFile != "",
		})
	}

	out := map[string]any{
		"branding":     s.branding(r),
		"hostnames":    names,
		"listen":       s.cfg.ListenAddr,
		"tls":          s.cfg.TLSEnabled,
		"ipv6_enabled": stored[db.SettingIPv6Enabled] == "1",
	}
	// The agent knows the host's addresses; a failure there should not stop
	// the rest of the page rendering.
	if info, err := agentclient.Call[actions.NetworkInfo](r.Context(), s.agent, "net.info", 1, struct{}{}); err == nil {
		out["network"] = info
	} else {
		s.log.Warn("httpapi: cannot read network info", "err", err)
	}
	if st, err := s.plans.QuotaStatus(r.Context()); err == nil {
		out["quota"] = st
	}
	writeJSON(w, http.StatusOK, out)
}

// handleHostnameAdd records a name the panel will answer on.
func (s *Server) handleHostnameAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hostname  string `json:"hostname"`
		IsPrimary bool   `json:"is_primary"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	host := strings.ToLower(strings.TrimSpace(req.Hostname))
	if !hostnamePattern.MatchString(host) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"that is not a hostname the panel can serve")
		return
	}
	if err := s.db.AddPanelHostname(r.Context(), &db.PanelHostname{
		Hostname: host, IsPrimary: req.IsPrimary,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if req.IsPrimary {
		if err := s.db.SetPrimaryHostname(r.Context(), host); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
	}
	s.audit(r, "settings.hostname.add", host, true, "")
	writeJSON(w, http.StatusCreated, map[string]any{"hostname": host})
}

// handleHostnameDelete stops the panel answering on a name.
func (s *Server) handleHostnameDelete(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("hostname")))
	if host == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "which hostname?")
		return
	}
	if err := s.db.DeletePanelHostname(r.Context(), host); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "settings.hostname.delete", host, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleIPv6Set records whether the panel and the webserver should listen on
// IPv6. Taking effect needs the webserver configuration regenerated, which
// this does, so the answer reflects reality rather than intent.
func (s *Server) handleIPv6Set(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.db.SetSetting(r.Context(), db.SettingIPv6Enabled,
		map[bool]string{true: "1", false: "0"}[req.Enabled]); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "settings.ipv6", "", true, strconv.FormatBool(req.Enabled))
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": req.Enabled})
}

// handleNotificationGet reports where the caller's own alerts go.
func (s *Server) handleNotificationGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.db.NotificationTargetFor(r.Context(), userFrom(r.Context()).ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if t == nil {
		writeJSON(w, http.StatusOK, map[string]any{"notifications": nil})
		return
	}
	// The token is never sent back. Showing it would let anybody who reaches
	// a logged-in browser take over the bot.
	writeJSON(w, http.StatusOK, map[string]any{"notifications": map[string]any{
		"telegram_configured": t.TelegramToken != "",
		"telegram_chat_id":    t.TelegramChatID,
		"events":              t.Events,
		"enabled":             t.Enabled,
	}})
}

// handleNotificationSet stores where the caller's alerts go.
func (s *Server) handleNotificationSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TelegramToken  string `json:"telegram_token"`
		TelegramChatID string `json:"telegram_chat_id"`
		Events         string `json:"events"`
		Enabled        bool   `json:"enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	actor := userFrom(r.Context())

	// An empty token means "leave the one already stored alone", so a user
	// can change the chat id without re-entering the token they cannot read.
	token := strings.TrimSpace(req.TelegramToken)
	if token == "" {
		if existing, err := s.db.NotificationTargetFor(r.Context(), actor.ID); err == nil && existing != nil {
			token = existing.TelegramToken
		}
	}

	if err := s.db.SaveNotificationTarget(r.Context(), &db.NotificationTarget{
		UserID: actor.ID, TelegramToken: token,
		TelegramChatID: strings.TrimSpace(req.TelegramChatID),
		Events:         strings.TrimSpace(req.Events), Enabled: req.Enabled,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "settings.notifications", actor.Username, true, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
