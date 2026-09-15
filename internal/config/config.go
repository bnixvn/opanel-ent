// Package config loads and validates runtime configuration.
//
// Everything comes from the environment so the same binary behaves the same
// whether it is started by systemd, by opanelctl, or by hand in a test.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved configuration for every OPanel binary. Each binary
// uses the subset it cares about; validation is shared so a typo in a unit
// file fails at startup rather than at first use.
type Config struct {
	// Env is "dev" or "prod". Dev relaxes cookie security so the panel can be
	// reached over plain HTTP from a workstation.
	Env string

	DataDir     string // /var/lib/opanel
	DBPath      string // <DataDir>/opanel.db
	AgentSocket string // /run/opanel/agent.sock

	ListenAddr string // host:port for the API listener
	PanelHost  string // hostname used in absolute URLs and cookie scoping

	// TLS is on by default. The panel carries administrator credentials over
	// the public internet, so plain HTTP is only acceptable on loopback.
	// When no certificate is supplied one is generated and self-signed --
	// a browser warning beats a password in clear text, and Let's Encrypt
	// needs a resolvable hostname the panel may not have yet.
	TLSEnabled  bool
	TLSCertFile string
	TLSKeyFile  string
	TLSDir      string

	SessionTTL     time.Duration
	SessionIdleTTL time.Duration

	// WebserverBackend is only what the panel was configured with. The
	// running server is whatever the agent reports, which is authoritative:
	// an operator can switch webserver from the panel, and this file is not
	// rewritten when they do. Kept as the fallback for the moment before the
	// agent has answered.
	WebserverBackend string
	// PHPProvider selects the PHP source. Empty means the one that goes with
	// the active webserver, which is the right answer on every host.
	PHPProvider string

	// ACMEEmail is the contact on the Let's Encrypt account. Optional, but
	// without it the CA cannot warn you that a certificate is about to
	// expire — the last line of defence when automated renewal breaks.
	ACMEEmail string

	LogLevel string // debug|info|warn|error
}

const (
	EnvDev  = "dev"
	EnvProd = "prod"

	BackendApache = "apache"
	BackendOLS    = "ols"
	BackendLSWS   = "lsws"

	ProviderLSPHP  = "lsphp"
	ProviderRemi   = "remi"
	ProviderAltPHP = "altphp"
)

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Env:              env("OPANEL_ENV", EnvProd),
		DataDir:          env("OPANEL_DATA_DIR", "/var/lib/opanel"),
		AgentSocket:      env("OPANEL_AGENT_SOCKET", "/run/opanel/agent.sock"),
		ListenAddr:       env("OPANEL_LISTEN", "0.0.0.0:2222"),
		TLSCertFile:      env("OPANEL_TLS_CERT", ""),
		TLSKeyFile:       env("OPANEL_TLS_KEY", ""),
		PanelHost:        env("OPANEL_PANEL_HOST", ""),
		WebserverBackend: env("OPANEL_WEBSERVER_BACKEND", BackendApache),
		PHPProvider:      env("OPANEL_PHP_PROVIDER", ""),
		ACMEEmail:        env("OPANEL_ACME_EMAIL", ""),
		LogLevel:         env("OPANEL_LOG_LEVEL", "info"),
	}
	c.DBPath = env("OPANEL_DB_PATH", filepath.Join(c.DataDir, "opanel.db"))
	c.TLSDir = env("OPANEL_TLS_DIR", filepath.Join(c.DataDir, "tls"))
	c.TLSEnabled = envBool("OPANEL_TLS", true)

	var err error
	if c.SessionTTL, err = envDuration("OPANEL_SESSION_TTL", 12*time.Hour); err != nil {
		return nil, err
	}
	if c.SessionIdleTTL, err = envDuration("OPANEL_SESSION_IDLE_TTL", 2*time.Hour); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate rejects combinations that would fail later in confusing ways.
func (c *Config) Validate() error {
	var errs []error
	switch c.Env {
	case EnvDev, EnvProd:
	default:
		errs = append(errs, fmt.Errorf("OPANEL_ENV: want %q or %q, got %q", EnvDev, EnvProd, c.Env))
	}
	switch c.WebserverBackend {
	case BackendApache, BackendOLS, BackendLSWS:
	default:
		errs = append(errs, fmt.Errorf("OPANEL_WEBSERVER_BACKEND: want %q, %q or %q, got %q",
			BackendApache, BackendOLS, BackendLSWS, c.WebserverBackend))
	}
	switch c.PHPProvider {
	case "", ProviderLSPHP, ProviderRemi, ProviderAltPHP:
	default:
		errs = append(errs, fmt.Errorf("OPANEL_PHP_PROVIDER: want %q, %q, %q or empty, got %q",
			ProviderLSPHP, ProviderRemi, ProviderAltPHP, c.PHPProvider))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("OPANEL_LOG_LEVEL: unknown level %q", c.LogLevel))
	}
	if c.DataDir == "" {
		errs = append(errs, errors.New("OPANEL_DATA_DIR must not be empty"))
	}
	if !strings.Contains(c.ListenAddr, ":") {
		errs = append(errs, fmt.Errorf("OPANEL_LISTEN: want host:port, got %q", c.ListenAddr))
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		errs = append(errs, errors.New("OPANEL_TLS_CERT and OPANEL_TLS_KEY must be set together"))
	}
	if c.SessionIdleTTL > c.SessionTTL {
		errs = append(errs, fmt.Errorf("OPANEL_SESSION_IDLE_TTL (%s) must not exceed OPANEL_SESSION_TTL (%s)",
			c.SessionIdleTTL, c.SessionTTL))
	}
	return errors.Join(errs...)
}

// SecureCookies reports whether session cookies get the Secure attribute.
//
// Tied to TLS rather than to the environment: a Secure cookie is simply not
// sent over plain HTTP, so setting it on a non-TLS listener locks everyone out
// instead of protecting them.
func (c *Config) SecureCookies() bool { return c.TLSEnabled && c.Env == EnvProd }

// Scheme is the URL scheme the panel answers on.
func (c *Config) Scheme() string {
	if c.TLSEnabled {
		return "https"
	}
	return "http"
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	case "1", "true", "yes", "on":
		return true
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Bare integers are read as seconds; systemd unit files often use them.
		if n, e2 := strconv.Atoi(v); e2 == nil {
			return time.Duration(n) * time.Second, nil
		}
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

// PanelPort is the port the API listens on, as a number.
//
// The firewall needs it: whatever else an operator closes, the port they are
// reading the panel through has to stay open, or the next change locks them
// out with no way back in.
func (c *Config) PanelPort() int {
	_, port, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return 2222
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return 2222
	}
	return n
}
