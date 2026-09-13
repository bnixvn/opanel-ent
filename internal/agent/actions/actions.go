// Package actions registers the privileged operations opanel-agent serves.
//
// One file per subject area. Every input struct validates itself, so a
// handler body can assume its arguments are already shaped correctly.
package actions

import (
	"context"
	"os"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/distro"
	"github.com/bnixvn/opanel-ent/internal/version"
)

// RegisterAll wires every action into r.
func RegisterAll(r *agent.Registry) {
	registerCore(r)
	registerSystemd(r)
	registerPackages(r)
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
}

func registerCore(r *agent.Registry) {
	agent.Register(r, "ping", 1, func(context.Context, struct{}) (PingResult, error) {
		return PingResult{Agent: "opanel-agent", Version: version.String(), PID: os.Getpid()}, nil
	})

	agent.Register(r, "sysinfo", 1, func(ctx context.Context, _ struct{}) (SysInfoResult, error) {
		return SysInfoResult{Distro: distro.Detect(ctx)}, nil
	})

	// Reports what this agent build actually serves, so opanelctl and the
	// api can detect a version skew instead of guessing from their own
	// compiled-in list.
	agent.Register(r, "agent.actions", 1, func(context.Context, struct{}) (map[string]int, error) {
		return r.Actions(), nil
	})
}
