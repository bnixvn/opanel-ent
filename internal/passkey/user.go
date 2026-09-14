package passkey

import (
	"encoding/base64"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/bnixvn/opanel-ent/internal/db"
)

// User adapts a panel account to what the WebAuthn library expects.
type User struct {
	Account     *db.User
	Credentials []*db.Passkey
}

// WebAuthnID is the stable handle the authenticator stores.
//
// The numeric account id, not the username: a passkey survives a rename, and
// binding it to a name that can change would silently orphan every key the
// first time somebody was renamed.
func (u *User) WebAuthnID() []byte {
	return []byte(itoa(u.Account.ID))
}

func (u *User) WebAuthnName() string { return u.Account.Username }

func (u *User) WebAuthnDisplayName() string {
	if u.Account.Email != "" {
		return u.Account.Username + " (" + u.Account.Email + ")"
	}
	return u.Account.Username
}

// WebAuthnCredentials converts the stored rows.
func (u *User) WebAuthnCredentials() []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(u.Credentials))
	for _, k := range u.Credentials {
		id, err := base64.RawURLEncoding.DecodeString(k.ID)
		if err != nil {
			continue
		}
		key, err := base64.StdEncoding.DecodeString(k.PublicKey)
		if err != nil {
			continue
		}
		var aaguid []byte
		if k.AAGUID != "" {
			aaguid, _ = base64.StdEncoding.DecodeString(k.AAGUID)
		}
		c := webauthn.Credential{
			ID:        id,
			PublicKey: key,
			Authenticator: webauthn.Authenticator{
				AAGUID:    aaguid,
				SignCount: k.SignCount,
			},
			Transport: transports(k.Transports),
		}
		// The library refuses a sign-in when these disagree with the
		// assertion, so what is stored has to be what was registered. A row
		// that predates the columns knows neither, and the caller adopts
		// them from the first assertion instead -- see AdoptUnknownFlags.
		if k.BackupEligible != nil {
			c.Flags.BackupEligible = *k.BackupEligible
		}
		if k.BackupState != nil {
			c.Flags.BackupState = *k.BackupState
		}
		out = append(out, c)
	}
	return out
}

// Row turns a freshly created credential into a storable row.
func Row(userID int64, label string, c *webauthn.Credential) *db.Passkey {
	names := make([]string, 0, len(c.Transport))
	for _, t := range c.Transport {
		names = append(names, string(t))
	}
	be, bs := c.Flags.BackupEligible, c.Flags.BackupState
	return &db.Passkey{
		BackupEligible: &be,
		BackupState:    &bs,
		ID:             base64.RawURLEncoding.EncodeToString(c.ID),
		UserID:         userID,
		Label:          label,
		PublicKey:      base64.StdEncoding.EncodeToString(c.PublicKey),
		AAGUID:         base64.StdEncoding.EncodeToString(c.Authenticator.AAGUID),
		SignCount:      c.Authenticator.SignCount,
		Resident:       c.Flags.UserPresent && c.Flags.UserVerified,
		Transports:     strings.Join(names, " "),
	}
}

func transports(s string) []protocol.AuthenticatorTransport {
	if s == "" {
		return nil
	}
	parts := strings.Fields(s)
	out := make([]protocol.AuthenticatorTransport, 0, len(parts))
	for _, p := range parts {
		out = append(out, protocol.AuthenticatorTransport(p))
	}
	return out
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// AdoptUnknownFlags fills in the backup flags of credentials registered
// before they were stored.
//
// Those rows know nothing, and the library compares what it is given against
// what the assertion carries -- so left at their zero value they claim every
// credential is backup-ineligible, which every phone passkey contradicts.
// Taking the values from the assertion is trust on first use, and costs
// nothing: these flags describe where a key is kept, not who is presenting
// it, and the signature is still checked against the public key registered
// for this account.
//
// Returns the values used, so the caller can write them down and not have to
// trust anything a second time.
func AdoptUnknownFlags(u *User, eligible, state bool) (adopted bool) {
	for _, k := range u.Credentials {
		if k.BackupEligible == nil {
			e := eligible
			k.BackupEligible = &e
			adopted = true
		}
		if k.BackupState == nil {
			st := state
			k.BackupState = &st
			adopted = true
		}
	}
	return adopted
}
