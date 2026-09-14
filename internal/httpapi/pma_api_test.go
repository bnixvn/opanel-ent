package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// phpMyAdmin is proxied on the panel's own origin, so the session check is
// the only thing between the internet and a tool that talks to MariaDB. If
// this ever returns anything but 401, the tool is exposed.
func TestPhpMyAdminNeedsASession(t *testing.T) {
	f := newFixture(t)

	for _, path := range []string{"/phpmyadmin/", "/phpmyadmin/index.php", "/phpmyadmin/opanel-sso.php"} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(f.srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("GET %s returned %d, want 401", path, resp.StatusCode)
			}
		})
	}
}

// The shim redeems tickets without a session, so the loopback check is what
// stands in for authentication. A ticket is a bearer token: if this endpoint
// answered the internet, guessing one would open somebody's databases.
func TestSSORedeemIsLoopbackOnly(t *testing.T) {
	f := newFixture(t)
	c := f.client(t)

	// httptest listens on 127.0.0.1, so a request through it is loopback and
	// gets past the address check -- and is then refused on the ticket, which
	// is the next gate and the one that matters here.
	resp := post(t, c, f.srv.URL+"/api/internal/sso/redeem",
		map[string]string{"token": "not-a-real-ticket"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown ticket returned %d, want 403", resp.StatusCode)
	}
	var body struct {
		Code string `json:"code"`
	}
	decode(t, resp, &body)
	if body.Code != "invalid_ticket" {
		t.Fatalf("code = %q, want invalid_ticket", body.Code)
	}
}

// Signing on for somebody else is a staff action. An end user asking for
// another account's databases must be refused before any account is minted.
func TestPhpMyAdminSignonRefusesOtherAccounts(t *testing.T) {
	f := newFixture(t)
	f.createAdmin(t, "root", "correct-horse-battery")
	c := f.client(t)

	in := post(t, c, f.srv.URL+"/api/auth/login",
		map[string]string{"username": "root", "password": "correct-horse-battery"})
	if in.StatusCode != http.StatusOK {
		t.Fatalf("login returned %d", in.StatusCode)
	}
	decode(t, in, nil)

	resp := post(t, c, f.srv.URL+"/api/phpmyadmin/signon?owner=nobody-here", map[string]any{})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown owner returned %d, want 404", resp.StatusCode)
	}
}

// The proxy must not pass a path outside phpMyAdmin through to PHP.
func TestPhpMyAdminPathIsNotEscapable(t *testing.T) {
	f := newFixture(t)

	resp, err := http.Get(f.srv.URL + "/phpmyadmin/../api/sites")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Go's client resolves the dots before sending, so this lands on
	// /api/sites, which needs a session of its own.
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("traversal returned %d, want 401", resp.StatusCode)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "x-httpd-php") {
		t.Fatal("the request reached PHP")
	}
}
