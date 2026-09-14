package httpapi

import (
	"bytes"
	"net/http"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/passkey"
)

// secondFactor decides what else an account needs after its password, and
// checks it.
//
// A passkey is a second step here, never a way in on its own: the account is
// named and its password proven before any authenticator is asked for
// anything. That also avoids the failure a passkey-only sign-in has, where
// the browser is asked to find a credential for this site and quietly offers
// nothing because the authenticator stored it against the account rather
// than discoverably.
//
// Returns true when the caller may proceed to create a session. When it
// returns false it has already written the response.
func (s *Server) secondFactor(w http.ResponseWriter, r *http.Request, user *db.User, req loginRequest) bool {
	keys, err := s.passkeysUsable(r, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return false
	}

	// A passkey, when the account has one: it is the stronger factor and the
	// one the account holder registered most recently.
	if len(keys) > 0 {
		return s.checkPasskeyFactor(w, r, user, keys, req)
	}

	if user.TOTPEnabled {
		if req.Code == "" {
			s.audit(r, "auth.login", user.Username, false, "second factor required")
			writeError(w, http.StatusUnauthorized, "totp_required", "two-factor code required")
			return false
		}
		if err := s.auth.CheckSecondFactor(r.Context(), user, req.Code); err != nil {
			s.audit(r, "auth.login", user.Username, false, "second factor rejected")
			writeError(w, http.StatusUnauthorized, "totp_invalid", "invalid two-factor code")
			return false
		}
	}
	return true
}

// passkeysUsable returns the account's passkeys, or none when the server
// cannot use them.
func (s *Server) passkeysUsable(r *http.Request, user *db.User) ([]*db.Passkey, error) {
	state := s.passkeyReadiness(r.Context())
	if !state.Ready || !state.Enabled {
		return nil, nil
	}
	return s.db.PasskeysFor(r.Context(), user.ID)
}

// checkPasskeyFactor asks for a passkey, or verifies the one that came back.
func (s *Server) checkPasskeyFactor(w http.ResponseWriter, r *http.Request, user *db.User, keys []*db.Passkey, req loginRequest) bool {
	wa, err := s.webauthnFor(r.Context())
	if err != nil {
		// The server stopped being able to do this between the list and now.
		// Falling back to the code the account also has is better than
		// locking somebody out of their own panel.
		if user.TOTPEnabled && req.Code != "" &&
			s.auth.CheckSecondFactor(r.Context(), user, req.Code) == nil {
			return true
		}
		writeError(w, http.StatusConflict, "not_ready",
			"this server cannot check passkeys at the moment")
		return false
	}
	wu := &passkey.User{Account: user, Credentials: keys}

	// Nothing came back yet: start the ceremony and tell the browser which
	// credentials this account has.
	if req.PasskeyChallengeID == "" || len(req.PasskeyCredential) == 0 {
		options, session, err := wa.BeginLogin(wu,
			webauthn.WithUserVerification(protocol.VerificationPreferred))
		if err != nil {
			s.log.Error("httpapi: begin passkey second factor", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not ask for your passkey")
			return false
		}
		id, err := s.storeChallenge(r.Context(), user.ID, "login", session)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return false
		}
		s.audit(r, "auth.login", user.Username, false, "passkey required")
		writeJSON(w, http.StatusUnauthorized, errorBody{
			Error: "confirm with your passkey",
			Code:  "passkey_required",
			Details: map[string]any{
				"challenge_id": id,
				"options":      options,
				// So the form can offer the code instead when the passkey is
				// on a device the person does not have with them.
				"totp_available": user.TOTPEnabled,
			},
		})
		return false
	}

	session, ok := s.takeChallenge(w, r, req.PasskeyChallengeID, "login")
	if !ok {
		return false
	}
	if session.UserID != user.ID {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "that passkey was not accepted")
		return false
	}

	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.PasskeyCredential))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the authenticator's reply was unusable")
		return false
	}
	cred, err := wa.ValidateLogin(wu, *session.Data, parsed)
	if err != nil {
		s.audit(r, "auth.login", user.Username, false, "passkey rejected")
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "that passkey was not accepted")
		return false
	}
	// A counter that goes backwards is the one sign this protocol gives that
	// an authenticator has been cloned. Refusing is the whole value of it.
	if cred.Authenticator.CloneWarning {
		s.audit(r, "auth.login", user.Username, false, "passkey clone warning")
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "that passkey was not accepted")
		return false
	}
	_ = s.db.TouchPasskey(r.Context(), credentialID(cred), cred.Authenticator.SignCount)
	return true
}
