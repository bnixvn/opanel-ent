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
		out = append(out, webauthn.Credential{
			ID:        id,
			PublicKey: key,
			Authenticator: webauthn.Authenticator{
				AAGUID:    aaguid,
				SignCount: k.SignCount,
			},
			Transport: transports(k.Transports),
		})
	}
	return out
}

// Row turns a freshly created credential into a storable row.
func Row(userID int64, label string, c *webauthn.Credential) *db.Passkey {
	names := make([]string, 0, len(c.Transport))
	for _, t := range c.Transport {
		names = append(names, string(t))
	}
	return &db.Passkey{
		ID:         base64.RawURLEncoding.EncodeToString(c.ID),
		UserID:     userID,
		Label:      label,
		PublicKey:  base64.StdEncoding.EncodeToString(c.PublicKey),
		AAGUID:     base64.StdEncoding.EncodeToString(c.Authenticator.AAGUID),
		SignCount:  c.Authenticator.SignCount,
		Resident:   c.Flags.UserPresent && c.Flags.UserVerified,
		Transports: strings.Join(names, " "),
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
