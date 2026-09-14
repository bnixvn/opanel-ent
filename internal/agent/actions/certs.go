package actions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/acme"
	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// CertBudget bounds a DNS-01 order. Waiting for a Cloudflare record to
// publish and for the CA to see it takes minutes on a slow zone, and a
// wildcard that times out has still spent a rate-limit slot.
const CertBudget = 10 * time.Minute

// CertWildcardRequest asks for a certificate covering a domain and all its
// immediate subdomains.
type CertWildcardRequest struct {
	Domain  string `json:"domain"`
	CFToken string `json:"cf_token"`
	Email   string `json:"email,omitempty"`
	Staging bool   `json:"staging,omitempty"`
}

// Validate checks the domain and that a token was supplied.
func (r *CertWildcardRequest) Validate() error {
	if r.Domain == "" || strings.HasPrefix(r.Domain, "*.") {
		return errors.New("give the base domain, without the *. prefix")
	}
	if strings.TrimSpace(r.CFToken) == "" {
		return errors.New("a Cloudflare API token is required")
	}
	return nil
}

// CertManualRequest installs a certificate an operator bought elsewhere.
type CertManualRequest struct {
	Domain      string `json:"domain"`
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"private_key"`
	CABundle    string `json:"ca_bundle,omitempty"`
}

// Validate checks that the two required parts are present. Everything else
// -- expiry, whether the key matches, whether it covers the domain -- is
// checked by the store before anything is written.
func (r *CertManualRequest) Validate() error {
	if r.Domain == "" {
		return errors.New("which domain is this certificate for?")
	}
	if !strings.Contains(r.Certificate, "BEGIN CERTIFICATE") {
		return errors.New("the certificate does not look like PEM")
	}
	if !strings.Contains(r.PrivateKey, "PRIVATE KEY") {
		return errors.New("the private key does not look like PEM")
	}
	return nil
}

// CertNameRequest names a stored certificate.
type CertNameRequest struct {
	Name string `json:"name"`
}

// Validate keeps the name to something that can be a directory.
func (r *CertNameRequest) Validate() error {
	if r.Name == "" || strings.ContainsAny(r.Name, "/\\") || strings.Contains(r.Name, "..") {
		return fmt.Errorf("%q is not a certificate name", r.Name)
	}
	return nil
}

// CertCoverRequest asks which stored certificate already answers for a host.
type CertCoverRequest struct {
	Host string `json:"host"`
}

// Validate checks the hostname.
func (r *CertCoverRequest) Validate() error {
	if r.Host == "" {
		return errors.New("which hostname?")
	}
	return nil
}

// CertListResult is everything the panel holds.
type CertListResult struct {
	Certificates []acme.Stored `json:"certificates"`
}

// certManager builds the shared manager. Its paths are fixed constants, so
// every action that touches a certificate looks in the same place.
func certManager() *acme.Manager {
	return &acme.Manager{
		StateDir: CertStateDir,
		CertDir:  CertDir,
		Webroot:  webserver.ACMEWebroot,
		CADirURL: acme.ProductionCA,
	}
}

func registerCerts(r *agent.Registry) {
	m := certManager()

	agent.Register(r, "cert.list", 1, func(_ context.Context, _ struct{}) (CertListResult, error) {
		list, err := m.List()
		if err != nil {
			return CertListResult{}, err
		}
		if list == nil {
			list = []acme.Stored{}
		}
		return CertListResult{Certificates: list}, nil
	})

	agent.Register(r, "cert.covering", 1, func(_ context.Context, in CertCoverRequest) (*acme.Stored, error) {
		return m.FindCovering(in.Host)
	})

	agent.Register(r, "cert.manual", 1, func(_ context.Context, in CertManualRequest) (*acme.Stored, error) {
		st, err := m.InstallManual(in.Domain,
			[]byte(in.Certificate), []byte(in.PrivateKey), []byte(in.CABundle))
		if err != nil {
			return nil, err
		}
		if err := grantPanelAccess(st.CertFile, st.KeyFile); err != nil {
			return nil, err
		}
		return st, nil
	})

	agent.Register(r, "cert.delete", 1, func(_ context.Context, in CertNameRequest) (struct{}, error) {
		return struct{}{}, m.Remove(in.Name)
	})

	agent.RegisterSlow(r, "cert.wildcard", 1, CertBudget,
		func(_ context.Context, in CertWildcardRequest) (acme.Certificate, error) {
			// A separate manager so the account email and directory can be
			// per-request without mutating the shared one.
			mm := *m
			if in.Email != "" {
				mm.Email = in.Email
			}
			if in.Staging {
				mm.CADirURL = acme.StagingCA
			}
			cert, err := mm.IssueWildcard(in.Domain, in.CFToken)
			if err != nil {
				return acme.Certificate{}, err
			}
			if err := grantPanelAccess(cert.CertFile, cert.KeyFile); err != nil {
				return acme.Certificate{}, err
			}
			return *cert, nil
		})
}
