// Package svc controls systemd units.
package svc

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

const systemctl = "systemctl"

// unitPattern guards against a caller-supplied unit name turning into extra
// systemctl arguments. Even with argv execution, a name like "--now" would be
// read as a flag, so the shape is validated rather than the quoting.
var unitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@\-]{0,127}$`)

// ValidUnit reports whether name is a syntactically acceptable unit name.
func ValidUnit(name string) bool { return unitPattern.MatchString(name) }

// Status is a unit's current state.
type Status struct {
	Unit    string `json:"unit"`
	Active  string `json:"active"`  // active|inactive|failed|activating|unknown
	Enabled string `json:"enabled"` // enabled|disabled|static|masked|not-found
}

// Running reports whether the unit is active.
func (s Status) Running() bool { return s.Active == "active" }

func check(unit string) error {
	if !ValidUnit(unit) {
		return fmt.Errorf("svc: invalid unit name %q", unit)
	}
	return nil
}

// Get reports a unit's active and enabled state. systemctl exits non-zero for
// inactive or disabled units, which is information rather than an error, so
// the exit code is ignored and the printed word is used.
func Get(ctx context.Context, unit string) (Status, error) {
	if err := check(unit); err != nil {
		return Status{}, err
	}
	st := Status{Unit: unit, Active: "unknown", Enabled: "unknown"}

	if res, _ := run.Cmd(ctx, []string{systemctl, "is-active", unit}, run.AllowExit(1, 3, 4)); res.Output() != "" {
		st.Active = strings.TrimSpace(res.Output())
	}
	if res, _ := run.Cmd(ctx, []string{systemctl, "is-enabled", unit}, run.AllowExit(1)); res.Output() != "" {
		st.Enabled = strings.TrimSpace(res.Output())
	}
	return st, nil
}

func do(ctx context.Context, verb, unit string) error {
	if err := check(unit); err != nil {
		return err
	}
	_, err := run.Cmd(ctx, []string{systemctl, verb, unit}, run.Timeout(operationTimeout))
	return err
}

const operationTimeout = 120 * time.Second

// Start, Stop, Restart, Reload and ReloadOrRestart drive a unit.
func Start(ctx context.Context, unit string) error   { return do(ctx, "start", unit) }
func Stop(ctx context.Context, unit string) error    { return do(ctx, "stop", unit) }
func Restart(ctx context.Context, unit string) error { return do(ctx, "restart", unit) }
func Reload(ctx context.Context, unit string) error  { return do(ctx, "reload", unit) }

// ReloadOrRestart reloads when the unit supports it and restarts otherwise.
func ReloadOrRestart(ctx context.Context, unit string) error {
	return do(ctx, "reload-or-restart", unit)
}

// Enable turns on boot activation, optionally starting the unit now.
func Enable(ctx context.Context, unit string, now bool) error {
	if err := check(unit); err != nil {
		return err
	}
	argv := []string{systemctl, "enable"}
	if now {
		argv = append(argv, "--now")
	}
	argv = append(argv, unit)
	_, err := run.Cmd(ctx, argv, run.Timeout(operationTimeout))
	return err
}

// Disable turns off boot activation, optionally stopping the unit now.
func Disable(ctx context.Context, unit string, now bool) error {
	if err := check(unit); err != nil {
		return err
	}
	argv := []string{systemctl, "disable"}
	if now {
		argv = append(argv, "--now")
	}
	argv = append(argv, unit)
	_, err := run.Cmd(ctx, argv, run.Timeout(operationTimeout))
	return err
}

// DaemonReload re-reads unit files after the panel writes one.
func DaemonReload(ctx context.Context) error {
	_, err := run.Cmd(ctx, []string{systemctl, "daemon-reload"}, run.Timeout(operationTimeout))
	return err
}

// Journal returns the last n lines for a unit.
func Journal(ctx context.Context, unit string, lines int) (string, error) {
	if err := check(unit); err != nil {
		return "", err
	}
	if lines <= 0 || lines > 5000 {
		lines = 200
	}
	res, err := run.Cmd(ctx, []string{
		"journalctl", "-u", unit, "-n", fmt.Sprint(lines), "--no-pager", "--output", "short-iso",
	})
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}
