// Package config loads and validates runtime configuration.
//
// Everything comes from the environment so the same binary behaves the same
// whether it is started by systemd, by opanelctl, or by hand in a test.
package config

import (
	"errors"
	"fmt"
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

	ListenAddr string // host:port for the HTTP API
	PanelHost  string // hostname used in absolute URLs and cookie scoping

	SessionTTL     time.Duration
	SessionIdleTTL time.Duration

	// WebserverBackend selects the config renderer: "ols" or "lsws".
	WebserverBackend string
	// PHPProvider selects the PHP source: "lsphp" or "altphp". Empty means
	// autodetect (altphp when CloudLinux is present).
	PHPProvider string

	LogLevel string // debug|info|warn|error
}

const (
	EnvDev  = "dev"
	EnvProd = "prod"

	BackendOLS  = "ols"
	BackendLSWS = "lsws"

	ProviderLSPHP  = "lsphp"
	ProviderAltPHP = "altphp"
)

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Env:              env("OPANEL_ENV", EnvProd),
		DataDir:          env("OPANEL_DATA_DIR", "/var/lib/opanel"),
		AgentSocket:      env("OPANEL_AGENT_SOCKET", "/run/opanel/agent.sock"),
		ListenAddr:       env("OPANEL_LISTEN", "127.0.0.1:2222"),
		PanelHost:        env("OPANEL_PANEL_HOST", ""),
		WebserverBackend: env("OPANEL_WEBSERVER_BACKEND", BackendOLS),
		PHPProvider:      env("OPANEL_PHP_PROVIDER", ""),
		LogLevel:         env("OPANEL_LOG_LEVEL", "info"),
	}
	c.DBPath = env("OPANEL_DB_PATH", filepath.Join(c.DataDir, "opanel.db"))

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
	case BackendOLS, BackendLSWS:
	default:
		errs = append(errs, fmt.Errorf("OPANEL_WEBSERVER_BACKEND: want %q or %q, got %q",
			BackendOLS, BackendLSWS, c.WebserverBackend))
	}
	switch c.PHPProvider {
	case "", ProviderLSPHP, ProviderAltPHP:
	default:
		errs = append(errs, fmt.Errorf("OPANEL_PHP_PROVIDER: want %q, %q or empty, got %q",
			ProviderLSPHP, ProviderAltPHP, c.PHPProvider))
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
	if c.SessionIdleTTL > c.SessionTTL {
		errs = append(errs, fmt.Errorf("OPANEL_SESSION_IDLE_TTL (%s) must not exceed OPANEL_SESSION_TTL (%s)",
			c.SessionIdleTTL, c.SessionTTL))
	}
	return errors.Join(errs...)
}

// SecureCookies reports whether session cookies get the Secure attribute.
// Dev mode drops it so the panel works over plain HTTP on a workstation.
func (c *Config) SecureCookies() bool { return c.Env == EnvProd }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
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
