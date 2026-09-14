// Package acme obtains TLS certificates from Let's Encrypt.
//
// Issuance runs inside the panel rather than shelling out to certbot. That
// removes a Python dependency from a host that already runs CloudLinux's own
// Python tooling, and it means renewal, storage and the webserver reload are
// one code path instead of a script plus a deploy hook.
package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/http/webroot"
	"github.com/go-acme/lego/v4/registration"
)

// Directory URLs. Staging exists so a misconfigured host can be debugged
// without spending production rate limit, which is per registered domain per
// week and unforgiving.
const (
	ProductionCA = lego.LEDirectoryProduction
	StagingCA    = lego.LEDirectoryStaging
)

// RenewBefore is how long ahead of expiry a certificate is replaced. Let's
// Encrypt issues for 90 days; 30 leaves room for a fortnight of failures
// before anything is visible to a visitor.
const RenewBefore = 30 * 24 * time.Hour

// Manager issues and stores certificates.
type Manager struct {
	// StateDir holds the ACME account. Private to the panel.
	StateDir string
	// CertDir holds issued certificates, one directory per primary domain.
	CertDir string
	// Webroot is where HTTP-01 challenge files are written. The webserver
	// must already serve it for every hostname.
	Webroot string
	// Email is the ACME account contact. Optional -- ACME allows an account
	// with no contact -- but strongly recommended: it is how Let's Encrypt
	// warns you that a certificate is about to expire, which is the last line
	// of defence when automated renewal breaks.
	Email string
	// CADirURL selects production or staging.
	CADirURL string
}

// Certificate describes an issued certificate on disk.
type Certificate struct {
	Domains  []string  `json:"domains"`
	CertFile string    `json:"cert_file"`
	KeyFile  string    `json:"key_file"`
	NotAfter time.Time `json:"not_after"`
}

// NeedsRenewal reports whether the certificate is close enough to expiry.
func (c Certificate) NeedsRenewal() bool {
	return time.Now().After(c.NotAfter.Add(-RenewBefore))
}

// Files inside a certificate directory.
const (
	certFileName  = "fullchain.pem"
	keyFileName   = "privkey.pem"
	accountKey    = "account.key"
	accountRecord = "account.json"
)

// Issue obtains a certificate for domains, the first of which is the common
// name and the directory it is stored under.
func (m *Manager) Issue(ctx context.Context, domains []string) (*Certificate, error) {
	if len(domains) == 0 {
		return nil, errors.New("acme: no domain given")
	}
	for _, d := range domains {
		if err := validDomain(d); err != nil {
			return nil, err
		}
	}
	client, err := m.client()
	if err != nil {
		return nil, err
	}

	// The challenge directory must exist before the CA is told to look: a
	// missing directory turns into an opaque 404 during validation.
	challengeDir := filepath.Join(m.Webroot, ".well-known", "acme-challenge")
	if err := os.MkdirAll(challengeDir, 0o755); err != nil {
		return nil, fmt.Errorf("acme: create challenge directory: %w", err)
	}
	provider, err := webroot.NewHTTPProvider(m.Webroot)
	if err != nil {
		return nil, fmt.Errorf("acme: webroot provider: %w", err)
	}
	if err := client.Challenge.SetHTTP01Provider(provider); err != nil {
		return nil, fmt.Errorf("acme: set http-01 provider: %w", err)
	}

	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: domains,
		Bundle:  true, // one file with the chain, which is what a webserver wants
	})
	if err != nil {
		return nil, fmt.Errorf("acme: obtain certificate for %s: %w", strings.Join(domains, ", "), err)
	}
	_ = ctx

	dir := filepath.Join(m.CertDir, domains[0])
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)
	if err := os.WriteFile(certPath, res.Certificate, 0o644); err != nil {
		return nil, fmt.Errorf("acme: write certificate: %w", err)
	}
	if err := os.WriteFile(keyPath, res.PrivateKey, 0o600); err != nil {
		return nil, fmt.Errorf("acme: write private key: %w", err)
	}

	notAfter, err := expiryOf(res.Certificate)
	if err != nil {
		return nil, err
	}
	return &Certificate{Domains: domains, CertFile: certPath, KeyFile: keyPath, NotAfter: notAfter}, nil
}

// Load reads an already-issued certificate.
func (m *Manager) Load(domain string) (*Certificate, error) {
	dir := filepath.Join(m.CertDir, domain)
	certPath := filepath.Join(dir, certFileName)
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	notAfter, err := expiryOf(pemBytes)
	if err != nil {
		return nil, err
	}
	return &Certificate{
		Domains:  []string{domain},
		CertFile: certPath,
		KeyFile:  filepath.Join(dir, keyFileName),
		NotAfter: notAfter,
	}, nil
}

func (m *Manager) client() (*lego.Client, error) {
	acct, err := m.loadOrRegister()
	if err != nil {
		return nil, err
	}
	cfg := lego.NewConfig(acct)
	cfg.CADirURL = m.CADirURL
	if cfg.CADirURL == "" {
		cfg.CADirURL = ProductionCA
	}
	cfg.Certificate.KeyType = certcrypto.EC256

	client, err := lego.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("acme: new client: %w", err)
	}
	if acct.Registration == nil {
		reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return nil, fmt.Errorf("acme: register account: %w", err)
		}
		acct.Registration = reg
		if err := m.saveRegistration(reg); err != nil {
			return nil, err
		}
	}
	return client, nil
}

// account implements registration.User.
type account struct {
	Email        string
	Registration *registration.Resource
	key          crypto.PrivateKey
}

func (a *account) GetEmail() string                        { return a.Email }
func (a *account) GetRegistration() *registration.Resource { return a.Registration }
func (a *account) GetPrivateKey() crypto.PrivateKey        { return a.key }

// accountDir is where this manager's ACME account lives.
//
// Split two ways, and both splits are load-bearing.
//
// By directory, because an account registered with the production CA does
// not exist at the staging one: reusing the key there fails with "KeyID
// header contained an invalid account URL", which turns staging -- the safe
// way to test an issuance -- into the one thing that cannot work on a host
// that has already issued a real certificate.
//
// By contact, because Let's Encrypt sends the expiry warning to the account,
// not to the order. One shared account would send every customer's warning
// to whoever registered first, which is nobody useful. An account per
// contact is a handful of accounts on a busy host and is well inside the
// registration rate limit.
func (m *Manager) accountDir() string {
	env := "production"
	if strings.Contains(m.CADirURL, "staging") {
		env = "staging"
	}
	bucket := "no-contact"
	if m.Email != "" {
		sum := sha256.Sum256([]byte(strings.ToLower(m.Email)))
		bucket = hex.EncodeToString(sum[:8])
	}
	return filepath.Join(m.StateDir, env, bucket)
}

// loadOrRegister returns the ACME account, creating its key on first use.
//
// The account is reused across every certificate with the same contact.
// Registering a new one per certificate would work but burns a rate limit
// that exists precisely to discourage it.
func (m *Manager) loadOrRegister() (*account, error) {
	dir := m.accountDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("acme: create state directory: %w", err)
	}
	keyPath := filepath.Join(dir, accountKey)

	var key *ecdsa.PrivateKey
	data, err := os.ReadFile(keyPath)
	switch {
	case err == nil:
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, errors.New("acme: account key is not valid PEM")
		}
		if key, err = x509.ParseECPrivateKey(block.Bytes); err != nil {
			return nil, fmt.Errorf("acme: parse account key: %w", err)
		}
	case errors.Is(err, os.ErrNotExist):
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, fmt.Errorf("acme: generate account key: %w", err)
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath,
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, fmt.Errorf("acme: write account key: %w", err)
		}
	default:
		return nil, err
	}

	acct := &account{Email: m.Email, key: key}
	if raw, err := os.ReadFile(filepath.Join(dir, accountRecord)); err == nil {
		var reg registration.Resource
		if json.Unmarshal(raw, &reg) == nil && reg.URI != "" {
			acct.Registration = &reg
		}
	}
	return acct, nil
}

func (m *Manager) saveRegistration(reg *registration.Resource) error {
	raw, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.accountDir(), accountRecord), raw, 0o600)
}

func expiryOf(pemBytes []byte) (time.Time, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return time.Time{}, errors.New("acme: certificate is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("acme: parse certificate: %w", err)
	}
	return cert.NotAfter, nil
}

// validDomain rejects anything that is not a plain hostname. The value becomes
// a directory name and is handed to a remote CA, so wildcards, schemes and
// paths are refused rather than sanitised.
func validDomain(d string) error {
	if d == "" || len(d) > 253 {
		return fmt.Errorf("acme: %q is not a valid domain", d)
	}
	if strings.ContainsAny(d, "/\\ :*?\"<>|") || strings.HasPrefix(d, ".") || strings.Contains(d, "..") {
		return fmt.Errorf("acme: %q is not a valid domain", d)
	}
	if !strings.Contains(d, ".") {
		return fmt.Errorf("acme: %q has no dot; a certificate needs a fully qualified name", d)
	}
	return nil
}
