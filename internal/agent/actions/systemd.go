package actions

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/svc"
)

// managedUnits is the allowlist of units the panel may control.
//
// The unix socket already restricts *who* can call; this restricts *what*
// they can reach. Without it, a flaw anywhere in the API would let a caller
// stop sshd or systemd-journald through a perfectly well-formed request.
var managedUnits = []string{
	"httpd",   // Apache
	"lshttpd", // LiteSpeed Enterprise, when a licence is installed
	"mariadb",
	"valkey",
	"nftables",
	"opanel-api",
	"opanel-agent",
	"sshd",
	"crond",
	"clamd@scan",
	// One pool manager per PHP version. Listed rather than discovered
	// because the allowlist is the point: it is what stops a flaw anywhere
	// in the panel from reaching a unit nobody meant it to touch. A version
	// that is not installed is skipped by systemd.list, so this costs
	// nothing on a host that runs one of them.
	"php74-php-fpm",
	"php80-php-fpm",
	"php81-php-fpm",
	"php82-php-fpm",
	"php83-php-fpm",
	"php84-php-fpm",
	"php85-php-fpm",
}

func unitAllowed(name string) bool { return slices.Contains(managedUnits, name) }

// UnitRequest names a unit to inspect or act on.
type UnitRequest struct {
	Unit string `json:"unit"`
}

// Validate rejects malformed and unmanaged unit names.
func (r *UnitRequest) Validate() error {
	if !svc.ValidUnit(r.Unit) {
		return fmt.Errorf("unit %q is not a valid unit name", r.Unit)
	}
	if !unitAllowed(r.Unit) {
		return fmt.Errorf("unit %q is not managed by opanel", r.Unit)
	}
	return nil
}

// UnitActionRequest asks for a state change.
type UnitActionRequest struct {
	Unit   string `json:"unit"`
	Action string `json:"action"` // start|stop|restart|reload|reload-or-restart
}

var unitVerbs = []string{"start", "stop", "restart", "reload", "reload-or-restart"}

// Validate checks both the unit and the verb.
func (r *UnitActionRequest) Validate() error {
	if err := (&UnitRequest{Unit: r.Unit}).Validate(); err != nil {
		return err
	}
	if !slices.Contains(unitVerbs, r.Action) {
		return fmt.Errorf("action %q is not one of %v", r.Action, unitVerbs)
	}
	return nil
}

// JournalRequest asks for recent log lines.
type JournalRequest struct {
	Unit  string `json:"unit"`
	Lines int    `json:"lines"`
}

// Validate checks the unit and clamps the line count.
func (r *JournalRequest) Validate() error {
	if err := (&UnitRequest{Unit: r.Unit}).Validate(); err != nil {
		return err
	}
	if r.Lines <= 0 || r.Lines > 5000 {
		r.Lines = 200
	}
	return nil
}

// JournalResult carries log text.
type JournalResult struct {
	Text string `json:"text"`
}

// UnitListResult reports the status of every managed unit.
type UnitListResult struct {
	Units []svc.Status `json:"units"`
}

// idleFPM reports a pool manager that is stopped because no site asks for
// that PHP version.
func idleFPM(unit string, st svc.Status) bool {
	version, ok := fpmUnitVersion(unit)
	if !ok || st.Running() {
		return false
	}
	return phpmgr.NewRemi().PoolCount(version) == 0
}

// fpmUnitVersion turns "php84-php-fpm" back into "8.4".
func fpmUnitVersion(unit string) (string, bool) {
	rest, ok := strings.CutSuffix(unit, "-php-fpm")
	if !ok {
		return "", false
	}
	digits, ok := strings.CutPrefix(rest, "php")
	if !ok || len(digits) != 2 {
		return "", false
	}
	return digits[:1] + "." + digits[1:], true
}

func registerSystemd(r *agent.Registry) {
	agent.Register(r, "systemd.status", 1, func(ctx context.Context, in UnitRequest) (svc.Status, error) {
		return svc.Get(ctx, in.Unit)
	})

	agent.Register(r, "systemd.list", 1, func(ctx context.Context, _ struct{}) (UnitListResult, error) {
		out := UnitListResult{Units: make([]svc.Status, 0, len(managedUnits))}
		for _, u := range managedUnits {
			st, err := svc.Get(ctx, u)
			if err != nil {
				continue
			}
			// Units that are not installed are noise on a fresh host.
			if st.Enabled == "not-found" && st.Active != "active" {
				continue
			}
			// A PHP version nobody's site uses is deliberately stopped, and
			// showing it here as a stopped service reads as something to go
			// and fix. The PHP page is where a version's state belongs,
			// alongside the number of sites that explains it.
			if idleFPM(u, st) {
				continue
			}
			out.Units = append(out.Units, st)
		}
		return out, nil
	})

	agent.Register(r, "systemd.action", 1, func(ctx context.Context, in UnitActionRequest) (svc.Status, error) {
		var err error
		switch in.Action {
		case "start":
			err = svc.Start(ctx, in.Unit)
		case "stop":
			err = svc.Stop(ctx, in.Unit)
		case "restart":
			err = svc.Restart(ctx, in.Unit)
		case "reload":
			err = svc.Reload(ctx, in.Unit)
		case "reload-or-restart":
			err = svc.ReloadOrRestart(ctx, in.Unit)
		}
		if err != nil {
			return svc.Status{}, err
		}
		return svc.Get(ctx, in.Unit)
	})

	agent.Register(r, "systemd.journal", 1, func(ctx context.Context, in JournalRequest) (JournalResult, error) {
		text, err := svc.Journal(ctx, in.Unit, in.Lines)
		return JournalResult{Text: text}, err
	})
}
