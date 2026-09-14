// Package passkey implements WebAuthn sign-in for the panel.
//
// A passkey is a key pair whose private half never leaves the phone or
// security key that made it, and whose signature is bound to the site that
// asked for it. That second property is why it is worth having here: a
// hosting panel is a high-value login on a public address, and a passkey
// cannot be phished, replayed from a breach elsewhere, or typed into a
// convincing copy of the login page.
//
// The cost is that it only works under conditions the server has to be in
// first, which is why Ready exists and why an administrator has to switch it
// on rather than the panel assuming it will work.
package passkey

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/go-webauthn/webauthn/webauthn"
)

// SettingEnabled is the settings key holding whether passkeys are on.
const SettingEnabled = "passkey.enabled"

// Readiness reports whether passkeys can work on this host, and what is
// missing when they cannot.
//
// WebAuthn binds a credential to a "relying party id", which is a domain
// name. There is no way to make that an IP address, and browsers refuse the
// API outside a secure context. So a panel reached at https://203.0.113.7:2222
// cannot use passkeys at all -- not because the panel is being cautious, but
// because the browser will not offer it. An administrator being able to
// switch it on in that state would only produce a broken login page.
type Readiness struct {
	Ready bool `json:"ready"`
	// Hostname is the relying party id passkeys would be bound to.
	Hostname string `json:"hostname,omitempty"`
	// Origin is the address the browser must be at for a passkey to work.
	Origin string `json:"origin,omitempty"`
	// Blockers are the conditions that are not met, in plain words.
	Blockers []string `json:"blockers,omitempty"`
	// Enabled is the administrator's switch, independent of readiness: a
	// host can be ready and still have it off.
	Enabled bool `json:"enabled"`
}

// ErrNotReady is returned when a ceremony is attempted on a host that cannot
// support one.
var ErrNotReady = errors.New("passkey: this server is not set up for passkeys")

// Check works out whether this host can support passkeys.
//
// hostname is the panel's primary hostname; hasCert says whether a
// certificate is installed for it; port is the panel's port.
func Check(hostname string, hasCert bool, port int, enabled bool) Readiness {
	r := Readiness{Enabled: enabled}
	host := strings.TrimSpace(strings.ToLower(hostname))

	switch {
	case host == "":
		r.Blockers = append(r.Blockers,
			"The panel has no hostname configured. Add one under Settings; "+
				"a passkey is bound to a name, and there is no way to bind one to an address.")
	case net.ParseIP(host) != nil:
		r.Blockers = append(r.Blockers,
			"The panel's address is an IP address. WebAuthn credentials are bound "+
				"to a domain name, so a passkey cannot be created for "+host+".")
	case !strings.Contains(host, "."):
		r.Blockers = append(r.Blockers,
			"\""+host+"\" is not a fully qualified domain name. Browsers refuse to "+
				"create a passkey for a single-label name.")
	default:
		r.Hostname = host
	}

	if !hasCert {
		r.Blockers = append(r.Blockers,
			"The panel has no certificate. Browsers only offer passkeys over HTTPS "+
				"with a certificate they trust — a self-signed one will not do.")
	}

	if r.Hostname != "" && hasCert {
		r.Ready = true
		r.Origin = originFor(r.Hostname, port)
	}
	return r
}

// originFor builds the origin a browser will send, which has to match
// exactly: 443 is implied and must not appear, any other port must.
func originFor(host string, port int) string {
	if port == 443 || port == 0 {
		return "https://" + host
	}
	return fmt.Sprintf("https://%s:%d", host, port)
}

// New builds the WebAuthn handler for a ready host.
func New(r Readiness, displayName string) (*webauthn.WebAuthn, error) {
	if !r.Ready {
		return nil, ErrNotReady
	}
	if displayName == "" {
		displayName = "OPanel"
	}
	return webauthn.New(&webauthn.Config{
		RPDisplayName: displayName,
		RPID:          r.Hostname,
		// Only the panel's own origin. A list with more than one entry is
		// how a credential ends up usable from somewhere it should not be.
		RPOrigins: []string{r.Origin},
	})
}
