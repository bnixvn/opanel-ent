package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/passkey"
)

// ChallengeTTL is how long a half-finished ceremony is good for. Long enough
// to unlock a phone and reach for a fingerprint, short enough that an
// abandoned one is gone before anybody could look for it.
const ChallengeTTL = 5 * time.Minute

// passkeyReadiness works out whether this host can support passkeys.
func (s *Server) passkeyReadiness(ctx context.Context) passkey.Readiness {
	// The panel's own TLS settings come first, because that is the
	// certificate the browser will actually be shown. The hostname list is
	// about which names the panel answers on, which is a different question:
	// a name can be listed without the panel serving a certificate for it.
	hostname := strings.ToLower(strings.TrimSpace(s.cfg.PanelHost))
	hasCert := s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != ""

	if rows, err := s.db.ListPanelHostnames(ctx); err == nil {
		for _, h := range rows {
			if hostname == "" || (h.IsPrimary && h.CertFile != "") {
				hostname = h.Hostname
				hasCert = hasCert || h.CertFile != ""
			}
			if h.IsPrimary {
				break
			}
		}
	}
	enabled, _ := s.db.Setting(ctx, passkey.SettingEnabled, "")
	return passkey.Check(hostname, hasCert, s.cfg.PanelPort(), enabled == "1")
}

// webauthnFor builds the handler, refusing on a host that cannot support one.
func (s *Server) webauthnFor(ctx context.Context) (*webauthn.WebAuthn, error) {
	r := s.passkeyReadiness(ctx)
	if !r.Enabled {
		return nil, passkey.ErrNotReady
	}
	brand, _ := s.db.Setting(ctx, "brand.name", "OPanel")
	return passkey.New(r, brand)
}

// handlePasskeyStatus reports readiness. Administrators see the blockers so
// they know what to fix; everyone else only needs to know whether the button
// will work.
func (s *Server) handlePasskeyStatus(w http.ResponseWriter, r *http.Request) {
	state := s.passkeyReadiness(r.Context())
	actor := userFrom(r.Context())
	if actor == nil || !auth.Role(actor.Role).AtLeast(auth.RoleAdmin) {
		state.Blockers = nil
	}
	keys, _ := s.db.PasskeysFor(r.Context(), actor.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"passkeys": viewPasskeys(keys),
		"state":    state,
	})
}

// handlePasskeyEnable is the administrator's switch.
//
// Gated on readiness rather than trusted: turning it on where it cannot work
// would put a "Sign in with a passkey" button on the login page that fails
// in the browser, which looks like the panel is broken.
func (s *Server) handlePasskeyEnable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	state := s.passkeyReadiness(r.Context())
	if req.Enabled && !state.Ready {
		writeJSON(w, http.StatusConflict, errorBody{
			Error:   "this server cannot support passkeys yet",
			Code:    "not_ready",
			Details: map[string]any{"blockers": state.Blockers},
		})
		return
	}
	value := "0"
	if req.Enabled {
		value = "1"
	}
	if err := s.db.SetSetting(r.Context(), passkey.SettingEnabled, value); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "passkey.enable", state.Hostname, true, value)
	state.Enabled = req.Enabled
	writeJSON(w, http.StatusOK, map[string]any{"state": state})
}

// --- registration ---------------------------------------------------------

func (s *Server) handlePasskeyRegisterStart(w http.ResponseWriter, r *http.Request) {
	wa, err := s.webauthnFor(r.Context())
	if err != nil {
		writeError(w, http.StatusConflict, "not_ready",
			"passkeys are not switched on for this server")
		return
	}
	actor := userFrom(r.Context())
	keys, err := s.db.PasskeysFor(r.Context(), actor.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	user := &passkey.User{Account: actor, Credentials: keys}

	options, session, err := wa.BeginRegistration(user,
		// Excluding what is already registered stops the same phone being
		// added twice and shows the customer a useful message instead.
		webauthn.WithExclusions(credentialDescriptors(keys)),
		// Not a discoverable credential: this is a second factor, asked for
		// by an account the server has already identified, so there is
		// nothing for the authenticator to look up. Asking for one anyway
		// consumes a slot on a security key that has only a handful.
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementDiscouraged),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementDiscouraged,
			UserVerification: protocol.VerificationPreferred,
		}),
	)
	if err != nil {
		s.log.Error("httpapi: begin passkey registration", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not start registration")
		return
	}
	id, err := s.storeChallenge(r.Context(), actor.ID, "register", session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"challenge_id": id, "options": options})
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	wa, err := s.webauthnFor(r.Context())
	if err != nil {
		writeError(w, http.StatusConflict, "not_ready",
			"passkeys are not switched on for this server")
		return
	}
	actor := userFrom(r.Context())

	var req struct {
		ChallengeID string          `json:"challenge_id"`
		Label       string          `json:"label"`
		Credential  json.RawMessage `json:"credential"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	session, ok := s.takeChallenge(w, r, req.ChallengeID, "register")
	if !ok {
		return
	}
	if session.UserID != actor.ID {
		writeError(w, http.StatusForbidden, "forbidden", "that registration was not yours")
		return
	}

	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(req.Credential))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the authenticator's reply was unusable")
		return
	}
	keys, err := s.db.PasskeysFor(r.Context(), actor.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	cred, err := wa.CreateCredential(&passkey.User{Account: actor, Credentials: keys}, *session.Data, parsed)
	if err != nil {
		s.audit(r, "passkey.register", actor.Username, false, err.Error())
		writeError(w, http.StatusBadRequest, "rejected", "that passkey could not be verified")
		return
	}

	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "Passkey"
	}
	if len(label) > 60 {
		label = label[:60]
	}
	row := passkey.Row(actor.ID, label, cred)
	if err := s.db.CreatePasskey(r.Context(), row); err != nil {
		writeError(w, http.StatusConflict, "exists", "that authenticator is already registered")
		return
	}
	s.audit(r, "passkey.register", actor.Username, true, label)
	writeJSON(w, http.StatusOK, map[string]any{"passkey": viewPasskey(row)})
}

func (s *Server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	actor := userFrom(r.Context())
	id := r.URL.Query().Get("id")
	if err := s.db.DeletePasskey(r.Context(), actor.ID, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no such passkey on your account")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	s.audit(r, "passkey.delete", actor.Username, true, id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- sign-in --------------------------------------------------------------

// --- plumbing -------------------------------------------------------------

// storedSession is a challenge with its decoded payload.
type storedSession struct {
	UserID int64
	Data   *webauthn.SessionData
}

func (s *Server) storeChallenge(ctx context.Context, userID int64, purpose string, data *webauthn.SessionData) (string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	id, err := randomToken()
	if err != nil {
		return "", err
	}
	return id, s.db.CreatePasskeyChallenge(ctx, &db.PasskeyChallenge{
		ID: id, UserID: userID, Purpose: purpose,
		Session: string(raw), ExpiresAt: time.Now().Add(ChallengeTTL),
	})
}

func (s *Server) takeChallenge(w http.ResponseWriter, r *http.Request, id, purpose string) (storedSession, bool) {
	row, err := s.db.TakePasskeyChallenge(r.Context(), id, purpose)
	if err != nil {
		writeError(w, http.StatusBadRequest, "expired",
			"that took too long; start again")
		return storedSession{}, false
	}
	var data webauthn.SessionData
	if err := json.Unmarshal([]byte(row.Session), &data); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return storedSession{}, false
	}
	return storedSession{UserID: row.UserID, Data: &data}, true
}

func credentialDescriptors(keys []*db.Passkey) []protocol.CredentialDescriptor {
	out := make([]protocol.CredentialDescriptor, 0, len(keys))
	for _, k := range keys {
		id, err := base64.RawURLEncoding.DecodeString(k.ID)
		if err != nil {
			continue
		}
		out = append(out, protocol.CredentialDescriptor{
			Type:         protocol.PublicKeyCredentialType,
			CredentialID: id,
		})
	}
	return out
}

type passkeyView struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	Transports string    `json:"transports,omitempty"`
}

func viewPasskey(k *db.Passkey) passkeyView {
	return passkeyView{
		ID: k.ID, Label: k.Label, CreatedAt: k.CreatedAt,
		LastUsedAt: k.LastUsedAt, Transports: k.Transports,
	}
}

func viewPasskeys(keys []*db.Passkey) []passkeyView {
	out := make([]passkeyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, viewPasskey(k))
	}
	return out
}

// credentialID is the stored form of a credential's id.
func credentialID(c *webauthn.Credential) string {
	return base64.RawURLEncoding.EncodeToString(c.ID)
}
