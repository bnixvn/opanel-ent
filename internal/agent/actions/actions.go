// Package actions registers the privileged operations opanel-agent serves.
//
// One file per subject area. Every input struct validates itself, so a
// handler body can assume its arguments are already shaped correctly.
package actions

import (
	"context"
	"fmt"
	"os"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/distro"
	"github.com/bnixvn/opanel-ent/internal/version"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// Deps are the pluggable pieces the agent works through. Injecting them
// rather than constructing them here is what lets the same action set drive
// Apache, LiteSpeed Enterprise and OpenLiteSpeed, and PHP-FPM or lsphp, with
// no change to any handler.
//
// They are functions rather than values because the panel can switch
// webserver while the agent is running. An agent that resolved its backend
// once at startup would keep writing the previous server's configuration
// until somebody restarted it -- and the moment a switch happens is exactly
// when nobody wants to be told to restart the thing doing the switching.
type Deps struct {
	// Backend returns the webserver the host is currently set to run.
	Backend func() (webserver.Backend, error)
	// PHP returns the provider that goes with that webserver.
	PHP func() phpmgr.Provider
}

// RegisterAll wires every action into r.
func RegisterAll(r *agent.Registry, deps Deps) {
	registerCore(r, deps)
	registerSysStat(r)
	registerSystemd(r)
	registerPackages(r)
	registerSites(r, deps)
	registerWebserver(r, deps)
	registerLSWS(r)
	registerCloudLinux(r)
	registerPHPIni(r, deps)
	registerDatabases(r)
	registerQuota(r)
	registerXFSQuota(r)
	registerNetwork(r)
	registerLogs(r)
	registerCerts(r)
	registerFirewall(r)
	registerWAF(r)
	registerWAFRules(r)
	registerSiteMove(r)
	registerDBExport(r)
	registerPMA(r)
	registerFiles(r)
	registerFilesPlus(r)
	registerBackup(r)
	registerWordPress(r, deps)
	registerWPManage(r, deps)
	registerClamAV(r)
	registerCron(r)
	registerDestinations(r)
	registerHostImport(r)
}

// PingResult is the reply to "ping".
type PingResult struct {
	Agent   string `json:"agent"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// SysInfoResult reports what the agent sees about the host.
type SysInfoResult struct {
	Distro distro.Info `json:"distro"`
	Uptime string      `json:"uptime,omitempty"`
	// Webserver and PHPProvider are what this agent is actually driving,
	// which is the question the panel's System page is asking. The panel's
	// own configuration answers a different one: it holds "" for the PHP
	// provider, meaning "work it out", and printing that told an operator
	// their server had no PHP provider at all.
	Webserver   string `json:"webserver,omitempty"`
	PHPProvider string `json:"php_provider,omitempty"`
}

func registerCore(r *agent.Registry, deps Deps) {
	agent.Register(r, "ping", 1, func(context.Context, struct{}) (PingResult, error) {
		return PingResult{Agent: "opanel-agent", Version: version.String(), PID: os.Getpid()}, nil
	})

	agent.Register(r, "sysinfo", 1, func(ctx context.Context, _ struct{}) (SysInfoResult, error) {
		out := SysInfoResult{Distro: distro.Detect(ctx), Uptime: hostUptime()}
		if deps.Backend != nil {
			if ws, err := deps.Backend(); err == nil {
				out.Webserver = ws.Name()
			}
		}
		if deps.PHP != nil {
			out.PHPProvider = deps.PHP().Name()
		}
		return out, nil
	})

	// Reports what this agent build actually serves, so opanelctl and the
	// api can detect a version skew instead of guessing from their own
	// compiled-in list.
	agent.Register(r, "agent.actions", 1, func(context.Context, struct{}) (map[string]int, error) {
		return r.Actions(), nil
	})
}

// hostUptime is how long the machine has been up, in a form meant to be read
// rather than parsed.
func hostUptime() string {
	seconds := uptimeSeconds()
	if seconds <= 0 {
		return ""
	}
	d := seconds / 86400
	h := (seconds % 86400) / 3600
	m := (seconds % 3600) / 60
	if d > 0 {
		return fmt.Sprintf("%dd %dh", d, h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}
