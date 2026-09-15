package acme

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Preflight checks that every address a name resolves to can serve the
// challenge, before Let's Encrypt is asked.
//
// The failure this exists for, seen twice on one host: a name with two A
// records, one of them pointing somewhere else entirely. Let's Encrypt
// validates from several vantage points and hits both, so issuance fails with
// "During secondary validation: <the other IP>: 404" -- which reads like a
// webserver problem and is not one. The same two records then make the
// finished site fail in a browser about half the time, with
// ERR_SSL_PROTOCOL_ERROR, because the other host does not speak TLS for that
// name either.
//
// So the check is the same one the CA performs: put a token where the
// challenge goes, then fetch it back through each address in turn. Doing it
// ourselves costs one request per address and turns a confusing CA error into
// a sentence naming the record to delete.
//
// The rule is deliberately narrow, because a false refusal here blocks
// somebody from getting a certificate at all:
//
//   - some addresses serve the token and some do not -> refuse, and name the
//     ones that did not. Split DNS is the only thing that produces this.
//   - none of them do -> say nothing. A host behind NAT that cannot reach its
//     own public address (no hairpin), or one whose firewall blocks outbound
//     HTTP, looks exactly like this, and Let's Encrypt is the better judge.
//   - all of them do -> proceed.
func (m *Manager) Preflight(ctx context.Context, domains []string) error {
	for _, domain := range domains {
		// A wildcard is validated over DNS-01 and never fetched, so there is
		// nothing to check and nothing that could go wrong this way.
		if strings.HasPrefix(domain, "*.") {
			continue
		}
		if err := m.preflightOne(ctx, domain); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) preflightOne(ctx context.Context, domain string) error {
	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil {
		// Unresolvable is worth saying plainly: it is the other common reason
		// issuance fails, and the CA's version of the message is no clearer.
		return fmt.Errorf("acme: %s does not resolve, so Let's Encrypt cannot reach it: %w", domain, err)
	}
	if len(addrs) < 2 {
		// One address cannot disagree with itself. Skipping keeps the common
		// case free of an HTTP request that could only fail for reasons this
		// check is not allowed to act on.
		return nil
	}

	token, cleanup, err := m.placeToken()
	if err != nil {
		return err
	}
	defer cleanup()

	var bad []string
	good := 0
	for _, addr := range addrs {
		if fetchToken(ctx, addr, domain, token) {
			good++
		} else {
			bad = append(bad, addr)
		}
	}
	if good == 0 || len(bad) == 0 {
		return nil
	}

	sort.Strings(bad)
	return fmt.Errorf("acme: %s resolves to %d addresses and %s did not serve the "+
		"challenge. Let's Encrypt checks from several places and will hit that one, so "+
		"issuance would fail -- and a browser reaching it would get a TLS error even if "+
		"the certificate existed. Remove the DNS record for %s, then try again",
		domain, len(addrs), strings.Join(bad, ", "), strings.Join(bad, " and "))
}

// placeToken writes a random file where the challenge goes and returns a
// function that removes it.
func (m *Manager) placeToken() (string, func(), error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	// The same shape as a real challenge token, so anything matching on the
	// path -- a rewrite rule, a WAF -- treats it the same way.
	token := "opanel-preflight-" + hex.EncodeToString(raw)

	dir := filepath.Join(m.Webroot, ".well-known", "acme-challenge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("acme: create challenge directory: %w", err)
	}
	path := filepath.Join(dir, token)
	if err := os.WriteFile(path, []byte(token), 0o644); err != nil {
		return "", nil, fmt.Errorf("acme: write the preflight token: %w", err)
	}
	return token, func() { _ = os.Remove(path) }, nil
}

// fetchToken asks one address for the token, with the domain in the Host
// header, exactly as the CA would.
func fetchToken(ctx context.Context, addr, domain, token string) bool {
	// net.JoinHostPort rather than bracketing on a colon: an IPv6 literal
	// needs brackets and an address that already carries a port must not be
	// given them again. Deciding by ParseIP tells those two apart, which a
	// test for ":" cannot.
	host := addr
	if net.ParseIP(addr) != nil {
		host = net.JoinHostPort(addr, "80")
	}
	url := "http://" + host + "/.well-known/acme-challenge/" + token

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Host = domain

	// No redirects followed and no proxy: the question is what this address
	// serves, not where it would send somebody.
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(len(token))+16))
	return err == nil && strings.TrimSpace(string(body)) == token
}
