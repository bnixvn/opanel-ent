package acme

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Stored describes a certificate the panel holds, whatever put it there.
type Stored struct {
	// Name is the directory it lives under, which is the primary domain.
	Name     string    `json:"name"`
	Domains  []string  `json:"domains"`
	CertFile string    `json:"cert_file"`
	KeyFile  string    `json:"key_file"`
	NotAfter time.Time `json:"not_after"`
	Issuer   string    `json:"issuer"`
	// Wildcard is true when any of its names starts with "*.".
	Wildcard bool `json:"wildcard"`
	// Manual marks a certificate an operator uploaded rather than one the
	// panel obtained, which matters because the panel must not try to renew
	// it and must not silently replace it.
	Manual bool `json:"manual"`
}

// Covers reports whether this certificate is valid for a hostname.
//
// This is what makes "reuse the wildcard I already have" work: adding
// shop.example.com looks through the store for something that already
// answers for that name.
func (s Stored) Covers(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, name := range s.Domains {
		if hostMatches(host, strings.ToLower(name)) {
			return true
		}
	}
	return false
}

// hostMatches implements the one wildcard rule TLS uses: a leading "*."
// matches exactly one label, so *.example.com covers a.example.com but not
// b.a.example.com and not example.com itself.
func hostMatches(host, pattern string) bool {
	if pattern == host {
		return true
	}
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}
	suffix := pattern[1:] // ".example.com"
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	label := host[:len(host)-len(suffix)]
	return label != "" && !strings.Contains(label, ".")
}

// manualMarker sits beside a certificate an operator uploaded.
const manualMarker = "manual"

// List returns every certificate in the store, soonest to expire first.
func (m *Manager) List() ([]Stored, error) {
	entries, err := os.ReadDir(m.CertDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Stored
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		st, err := m.Describe(e.Name())
		if err != nil {
			// A half-written directory is not a reason to fail the listing.
			continue
		}
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NotAfter.Before(out[j].NotAfter) })
	return out, nil
}

// Describe reads one stored certificate.
func (m *Manager) Describe(name string) (*Stored, error) {
	dir := filepath.Join(m.CertDir, name)
	certPath := filepath.Join(dir, certFileName)
	body, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	leaf, err := leafOf(body)
	if err != nil {
		return nil, err
	}

	st := &Stored{
		Name:     name,
		CertFile: certPath,
		KeyFile:  filepath.Join(dir, keyFileName),
		NotAfter: leaf.NotAfter,
		Issuer:   leaf.Issuer.CommonName,
		Domains:  namesOf(leaf),
	}
	for _, d := range st.Domains {
		if strings.HasPrefix(d, "*.") {
			st.Wildcard = true
		}
	}
	if _, err := os.Stat(filepath.Join(dir, manualMarker)); err == nil {
		st.Manual = true
	}
	return st, nil
}

// FindCovering returns the stored certificate that already answers for a
// hostname, preferring the one that expires latest so a reused certificate
// does not need replacing next week.
func (m *Manager) FindCovering(host string) (*Stored, error) {
	all, err := m.List()
	if err != nil {
		return nil, err
	}
	var best *Stored
	for i := range all {
		c := all[i]
		if !c.Covers(host) || c.NotAfter.Before(time.Now()) {
			continue
		}
		if best == nil || c.NotAfter.After(best.NotAfter) {
			best = &c
		}
	}
	if best == nil {
		return nil, nil
	}
	return best, nil
}

// InstallManual validates and stores a certificate an operator supplied.
//
// Everything is checked before a byte is written. A certificate whose key
// does not match, or which does not cover the domain, would leave the
// webserver refusing to start after a reload -- which is a much worse
// failure than a rejected upload.
func (m *Manager) InstallManual(domain string, certPEM, keyPEM, chainPEM []byte) (*Stored, error) {
	if err := validDomain(domain); err != nil {
		return nil, err
	}
	leaf, err := leafOf(certPEM)
	if err != nil {
		return nil, fmt.Errorf("the certificate could not be read: %w", err)
	}
	if time.Now().After(leaf.NotAfter) {
		return nil, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.Format(time.DateOnly))
	}
	if time.Now().Before(leaf.NotBefore) {
		return nil, fmt.Errorf("the certificate is not valid until %s", leaf.NotBefore.Format(time.DateOnly))
	}
	if !coversHost(leaf, domain) {
		return nil, fmt.Errorf("the certificate does not cover %s (it covers %s)",
			domain, strings.Join(namesOf(leaf), ", "))
	}
	if err := keyMatchesCert(keyPEM, leaf); err != nil {
		return nil, err
	}
	// A chain that cannot be parsed would be served to browsers as garbage.
	if len(chainPEM) > 0 {
		if _, err := parseAll(chainPEM); err != nil {
			return nil, fmt.Errorf("the CA bundle could not be read: %w", err)
		}
	}

	// The webserver wants one file holding the leaf and then the chain.
	full := append([]byte(strings.TrimRight(string(certPEM), "\n")+"\n"), chainPEM...)

	st, err := m.store(domain, namesOf(leaf), full, keyPEM)
	if err != nil {
		return nil, err
	}
	// Marked so renewal leaves it alone: the panel has no way to obtain a
	// replacement for a certificate somebody bought.
	if err := os.WriteFile(filepath.Join(m.CertDir, domain, manualMarker),
		[]byte("uploaded\n"), 0o644); err != nil {
		return nil, err
	}
	return &Stored{
		Name: domain, Domains: st.Domains, CertFile: st.CertFile,
		KeyFile: st.KeyFile, NotAfter: st.NotAfter,
		Issuer: leaf.Issuer.CommonName, Manual: true,
	}, nil
}

// leafOf returns the first certificate in a PEM bundle.
func leafOf(body []byte) (*x509.Certificate, error) {
	certs, err := parseAll(body)
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, errors.New("no certificate found")
	}
	return certs[0], nil
}

func parseAll(body []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := body
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no PEM certificate block found")
	}
	return out, nil
}

func namesOf(c *x509.Certificate) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	// SAN is authoritative; the common name is a fallback for very old
	// certificates and is ignored by browsers when a SAN exists.
	for _, n := range c.DNSNames {
		add(n)
	}
	if len(out) == 0 {
		add(c.Subject.CommonName)
	}
	return out
}

func coversHost(c *x509.Certificate, host string) bool {
	for _, n := range namesOf(c) {
		if hostMatches(strings.ToLower(host), n) {
			return true
		}
	}
	return false
}

// keyMatchesCert checks that the private key belongs to the certificate.
//
// The comparison is on the public key rather than by attempting a signature:
// it covers every algorithm the same way and cannot be fooled by a key that
// happens to sign.
func keyMatchesCert(keyPEM []byte, leaf *x509.Certificate) error {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return errors.New("the private key is not a PEM file")
	}
	key, err := parsePrivateKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("the private key could not be read: %w", err)
	}

	type publicKeyed interface{ Equal(x any) bool }
	switch pub := leaf.PublicKey.(type) {
	case *rsa.PublicKey:
		k, ok := key.(*rsa.PrivateKey)
		if !ok || !pub.Equal(k.Public()) {
			return errors.New("the private key does not match the certificate")
		}
	case *ecdsa.PublicKey:
		k, ok := key.(*ecdsa.PrivateKey)
		if !ok || !pub.Equal(k.Public()) {
			return errors.New("the private key does not match the certificate")
		}
	case ed25519.PublicKey:
		k, ok := key.(ed25519.PrivateKey)
		if !ok || !pub.Equal(k.Public()) {
			return errors.New("the private key does not match the certificate")
		}
	default:
		return errors.New("the certificate uses a key type this panel does not handle")
	}
	return nil
}

func parsePrivateKey(der []byte) (any, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	return x509.ParseECPrivateKey(der)
}

// Remove deletes a stored certificate.
func (m *Manager) Remove(name string) error {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return fmt.Errorf("%q is not a certificate name", name)
	}
	return os.RemoveAll(filepath.Join(m.CertDir, name))
}
