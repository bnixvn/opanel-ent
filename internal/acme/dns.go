package acme

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
)

// IssueWildcard obtains a certificate covering *.domain and domain itself.
//
// A wildcard can only be validated over DNS-01: the CA will not accept an
// HTTP challenge for a name that stands for every subdomain, since serving a
// file on one host says nothing about the others. That is why this needs an
// API token and the HTTP path does not.
func (m *Manager) IssueWildcard(domain, cfToken string) (*Certificate, error) {
	if err := validDomain(domain); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfToken) == "" {
		return nil, errors.New("acme: a Cloudflare API token is required for a wildcard")
	}

	client, err := m.client()
	if err != nil {
		return nil, err
	}

	cfg := cloudflare.NewDefaultConfig()
	cfg.AuthToken = strings.TrimSpace(cfToken)
	// The default is two minutes, which is short for a zone whose
	// nameservers are slow to publish; a wildcard that fails at validation
	// costs a rate-limit slot, so waiting longer is the cheaper mistake.
	cfg.PropagationTimeout = 5 * time.Minute
	cfg.PollingInterval = 10 * time.Second

	provider, err := cloudflare.NewDNSProviderConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("acme: cloudflare provider: %w", err)
	}
	// Ask the authoritative nameservers directly rather than a recursive
	// resolver, which may still be serving a cached negative answer.
	if err := client.Challenge.SetDNS01Provider(provider,
		dns01.CondOption(true, dns01.DisableCompletePropagationRequirement())); err != nil {
		return nil, fmt.Errorf("acme: set dns-01 provider: %w", err)
	}

	domains := []string{domain, "*." + domain}
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: domains,
		Bundle:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("acme: obtain wildcard for %s: %w", domain, err)
	}
	return m.store(domain, domains, res.Certificate, res.PrivateKey)
}

// store writes an obtained certificate to its directory.
func (m *Manager) store(name string, domains []string, certPEM, keyPEM []byte) (*Certificate, error) {
	dir := filepath.Join(m.CertDir, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("acme: write certificate: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("acme: write private key: %w", err)
	}
	notAfter, err := expiryOf(certPEM)
	if err != nil {
		return nil, err
	}
	return &Certificate{Domains: domains, CertFile: certPath, KeyFile: keyPath, NotAfter: notAfter}, nil
}
