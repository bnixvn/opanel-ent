package passkey

import (
	"strings"
	"testing"
)

// The gate exists because a browser refuses WebAuthn in these cases. Letting
// an administrator switch it on anyway would put a button on the login page
// that fails inside the browser, which reads as the panel being broken.
func TestCheckRefusesWhatBrowsersRefuse(t *testing.T) {
	cases := []struct {
		name     string
		hostname string
		hasCert  bool
		blocker  string
	}{
		{"no hostname at all", "", true, "no hostname"},
		{"an IPv4 address", "203.0.113.7", true, "IP address"},
		{"an IPv6 address", "2001:db8::1", true, "IP address"},
		{"a single-label name", "panel", true, "fully qualified"},
		{"no certificate", "panel.example.com", false, "no certificate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(tc.hostname, tc.hasCert, 2222, false)
			if got.Ready {
				t.Fatalf("Check(%q, cert=%v) said ready", tc.hostname, tc.hasCert)
			}
			if len(got.Blockers) == 0 {
				t.Fatal("nothing was reported as blocking")
			}
			joined := strings.Join(got.Blockers, " ")
			if !strings.Contains(joined, tc.blocker) {
				t.Fatalf("blockers do not mention %q: %v", tc.blocker, got.Blockers)
			}
		})
	}
}

func TestCheckAcceptsAProperlyConfiguredHost(t *testing.T) {
	got := Check("Panel.Example.COM", true, 2222, true)
	if !got.Ready {
		t.Fatalf("a host with a name and a certificate was refused: %v", got.Blockers)
	}
	// The relying party id has to be lowercase, because that is what the
	// browser sends and the comparison is exact.
	if got.Hostname != "panel.example.com" {
		t.Fatalf("hostname = %q, want it lowercased", got.Hostname)
	}
	if !got.Enabled {
		t.Fatal("the administrator's switch was lost")
	}
}

// The origin has to match what the browser sends, byte for byte. A port of
// 443 is implied and must be absent; anything else must be present.
func TestOriginMatchesWhatTheBrowserSends(t *testing.T) {
	cases := map[int]string{
		2222: "https://panel.example.com:2222",
		443:  "https://panel.example.com",
		0:    "https://panel.example.com",
		8443: "https://panel.example.com:8443",
	}
	for port, want := range cases {
		got := Check("panel.example.com", true, port, true)
		if got.Origin != want {
			t.Errorf("port %d gave origin %q, want %q", port, got.Origin, want)
		}
	}
}

// Ready and Enabled are separate: a host can be capable and still have the
// feature switched off, and the login page must not offer it then.
func TestReadyIsNotEnabled(t *testing.T) {
	got := Check("panel.example.com", true, 2222, false)
	if !got.Ready {
		t.Fatal("the host should be ready")
	}
	if got.Enabled {
		t.Fatal("a ready host was reported as enabled")
	}
}

func TestNewRefusesAnUnreadyHost(t *testing.T) {
	if _, err := New(Check("203.0.113.7", true, 2222, true), "OPanel"); err == nil {
		t.Fatal("a WebAuthn handler was built for an IP address")
	}
}

func TestNewBindsToOneOrigin(t *testing.T) {
	wa, err := New(Check("panel.example.com", true, 2222, true), "OPanel")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// More than one origin is how a credential ends up usable from somewhere
	// it should not be.
	if len(wa.Config.RPOrigins) != 1 {
		t.Fatalf("got %d origins, want exactly one", len(wa.Config.RPOrigins))
	}
	if wa.Config.RPID != "panel.example.com" {
		t.Fatalf("RPID = %q", wa.Config.RPID)
	}
}
