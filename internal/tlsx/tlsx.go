// Package tlsx supplies the panel's own TLS certificate.
//
// The panel is reached over the public internet and carries administrator
// credentials, so it must not be served over plain HTTP. Let's Encrypt needs a
// resolvable hostname the panel may not have on day one, so a self-signed
// certificate is generated on first start and replaced later by a real one.
// A browser warning is a nuisance; a password in clear text is not.
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// File names inside the TLS directory.
const (
	CertFile = "panel-cert.pem"
	KeyFile  = "panel-key.pem"
)

// selfSignedLifetime is long enough that renewal is not a routine chore and
// short enough that a forgotten temporary certificate does not live forever.
const selfSignedLifetime = 5 * 365 * 24 * time.Hour

// LoadOrCreateSelfSigned returns a certificate for the panel, generating one
// if the directory does not already hold a usable pair.
//
// hosts are the names and addresses the certificate should cover. An empty
// list still yields a working certificate covering localhost.
func LoadOrCreateSelfSigned(dir string, hosts []string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, CertFile)
	keyPath := filepath.Join(dir, KeyFile)

	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if usable(cert) {
			return cert, nil
		}
		// Expired or unparseable: replaced rather than reported, since a
		// panel that will not start is worse than one with a fresh
		// self-signed certificate.
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return tls.Certificate{}, fmt.Errorf("tlsx: create %s: %w", dir, err)
	}
	certPEM, keyPEM, err := generate(hosts)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, fmt.Errorf("tlsx: write certificate: %w", err)
	}
	// 0600: the private key is the one file here that must not be readable by
	// anything else on the host.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("tlsx: write private key: %w", err)
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// usable reports whether a loaded certificate is still valid for a while.
func usable(cert tls.Certificate) bool {
	if len(cert.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return false
	}
	now := time.Now()
	return now.After(leaf.NotBefore) && now.Before(leaf.NotAfter.Add(-24*time.Hour))
}

func generate(hosts []string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("tlsx: generate key: %w", err)
	}
	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return nil, nil, fmt.Errorf("tlsx: generate serial: %w", err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "OPanel", Organization: []string{"OPanel"}},
		NotBefore:             time.Now().Add(-time.Hour), // tolerate a little clock skew
		NotAfter:              time.Now().Add(selfSignedLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	tmpl.DNSNames = append(tmpl.DNSNames, "localhost")
	tmpl.IPAddresses = append(tmpl.IPAddresses, net.IPv4(127, 0, 0, 1), net.IPv6loopback)
	for _, h := range hosts {
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("tlsx: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("tlsx: marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		nil
}

// ServerConfig returns a TLS configuration for the panel listener.
func ServerConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
}

// LocalAddresses returns the host's non-loopback addresses, so a generated
// certificate covers the address an operator will actually type.
func LocalAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipnet.IP.String())
	}
	return out
}
