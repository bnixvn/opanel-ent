// Package installer brings a bare AlmaLinux 10 host up to a running panel.
//
// Every step is idempotent and split into Check and Apply, so a run that is
// interrupted -- by a network failure, a reboot, an operator's Ctrl-C -- can
// simply be run again. That property matters more than speed: an installer
// that cannot be re-run leaves the operator repairing a half-built host by
// hand.
package installer

import (
	"context"
	"embed"
	"fmt"
	"io"
	"os"
	"time"
)

//go:embed assets
var assetsFS embed.FS

// Options configure an installation.
type Options struct {
	// PanelPort is where opanel-api listens.
	PanelPort int
	// BinDir is where the three binaries are copied from. Defaults to the
	// directory holding the running opanelctl.
	BinDir string
	// AdminUser is created when no panel user exists yet.
	AdminUser string
	// PHPVersions are installed; the first is the default for new sites.
	PHPVersions []string
	// SkipFirewall leaves nftables alone, for a host whose firewall is
	// managed elsewhere.
	SkipFirewall bool
	// Out receives progress.
	Out io.Writer
}

// Defaults fills in anything the caller left blank.
func (o *Options) Defaults() {
	if o.PanelPort == 0 {
		o.PanelPort = 2222
	}
	if o.AdminUser == "" {
		o.AdminUser = "admin"
	}
	if len(o.PHPVersions) == 0 {
		o.PHPVersions = []string{"8.4", "8.3"}
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.BinDir == "" {
		if exe, err := os.Executable(); err == nil {
			o.BinDir = dirOf(exe)
		}
	}
}

// Step is one unit of installation.
type Step struct {
	Name string
	// Check reports whether the step is already satisfied. A step with no
	// Check always runs; its Apply must be idempotent on its own.
	Check func(context.Context, *Options) (bool, error)
	// Apply performs the step.
	Apply func(context.Context, *Options) error
}

// Result records what one step did.
type Result struct {
	Name     string
	Skipped  bool
	Duration time.Duration
	Err      error
}

// Run executes every step in order, stopping at the first failure.
//
// Stopping rather than continuing is deliberate: later steps assume earlier
// ones succeeded, and pressing on would replace one clear error with a pile
// of confusing ones.
func Run(ctx context.Context, opts *Options) ([]Result, error) {
	opts.Defaults()
	steps := allSteps()
	results := make([]Result, 0, len(steps))

	for i, st := range steps {
		start := time.Now()
		fmt.Fprintf(opts.Out, "[%2d/%2d] %-42s", i+1, len(steps), st.Name)

		if st.Check != nil {
			done, err := st.Check(ctx, opts)
			if err != nil {
				fmt.Fprintf(opts.Out, "FAILED\n")
				results = append(results, Result{Name: st.Name, Duration: time.Since(start), Err: err})
				return results, fmt.Errorf("%s: %w", st.Name, err)
			}
			if done {
				fmt.Fprintf(opts.Out, "already done\n")
				results = append(results, Result{Name: st.Name, Skipped: true, Duration: time.Since(start)})
				continue
			}
		}

		if err := st.Apply(ctx, opts); err != nil {
			fmt.Fprintf(opts.Out, "FAILED\n")
			results = append(results, Result{Name: st.Name, Duration: time.Since(start), Err: err})
			return results, fmt.Errorf("%s: %w", st.Name, err)
		}
		fmt.Fprintf(opts.Out, "ok (%s)\n", time.Since(start).Round(100*time.Millisecond))
		results = append(results, Result{Name: st.Name, Duration: time.Since(start)})
	}
	return results, nil
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
