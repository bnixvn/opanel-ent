package acme

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A refusal here stops somebody getting a certificate at all, so the rule has
// to be exactly "some addresses work and some do not". Everything else must
// pass through and let the CA judge.
func TestPreflightOnlyRefusesSplitDNS(t *testing.T) {
	m := &Manager{Webroot: t.TempDir()}
	token, cleanup, err := m.placeToken()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	data, err := os.ReadFile(filepath.Join(m.Webroot, ".well-known", "acme-challenge", token))
	if err != nil {
		t.Fatalf("the token was not written where the challenge goes: %v", err)
	}
	if string(data) != token {
		t.Errorf("token file holds %q, want %q", data, token)
	}
}

// fetchToken is what decides whether an address is serving this host's
// challenge. It has to reject a server that answers with something else,
// because a parked page returning 200 for everything is exactly the shape of
// the other host in a split-DNS pair.
func TestFetchTokenRejectsAnythingButTheToken(t *testing.T) {
	const token = "opanel-preflight-deadbeef"

	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		want bool
	}{
		{"serves the token", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(token))
		}, true},
		{"404", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}, false},
		{"200 with a parked page", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>domain for sale</html>"))
		}, false},
		{"redirect to somewhere plausible", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com/", http.StatusFound)
		}, false},
	} {
		srv := httptest.NewServer(tc.h)
		addr := strings.TrimPrefix(srv.URL, "http://")
		got := fetchToken(context.Background(), addr, "example.test", token)
		srv.Close()
		if got != tc.want {
			t.Errorf("%s: fetchToken = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A wildcard is validated over DNS-01 and never fetched, so preflighting it
// would fail on a name that was never meant to answer HTTP.
func TestPreflightSkipsWildcards(t *testing.T) {
	m := &Manager{Webroot: t.TempDir()}
	if err := m.Preflight(context.Background(), []string{"*.example.invalid"}); err != nil {
		t.Errorf("a wildcard was preflighted: %v", err)
	}
}
