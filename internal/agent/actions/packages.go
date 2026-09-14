package actions

import (
	"context"
	"fmt"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/platform/pkgmgr"
)

// PackageQuery names a package or glob to look up.
type PackageQuery struct {
	Name string `json:"name"`
}

// Validate checks the name shape.
func (q *PackageQuery) Validate() error {
	if !pkgmgr.ValidName(q.Name) {
		return fmt.Errorf("package name %q is not valid", q.Name)
	}
	return nil
}

// PackageListResult carries a set of packages.
type PackageListResult struct {
	Packages []pkgmgr.Package `json:"packages"`
}

// PackageStatusResult reports whether a package is installed.
type PackageStatusResult struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
}

// UpgradeRequest asks for pending updates to be applied.
type UpgradeRequest struct {
	SecurityOnly bool `json:"security_only"`
}

func registerPackages(r *agent.Registry) {
	agent.Register(r, "pkg.status", 1, func(ctx context.Context, in PackageQuery) (PackageStatusResult, error) {
		v, err := pkgmgr.InstalledVersion(ctx, in.Name)
		if err != nil {
			return PackageStatusResult{}, err
		}
		return PackageStatusResult{Name: in.Name, Installed: v != "", Version: v}, nil
	})

	agent.Register(r, "pkg.available", 1, func(ctx context.Context, in PackageQuery) (PackageListResult, error) {
		pkgs, err := pkgmgr.Available(ctx, in.Name)
		return PackageListResult{Packages: pkgs}, err
	})

	agent.Register(r, "pkg.upgradable", 1, func(ctx context.Context, _ struct{}) (PackageListResult, error) {
		pkgs, err := pkgmgr.Upgradable(ctx)
		return PackageListResult{Packages: pkgs}, err
	})

	agent.Register(r, "pkg.upgradable_security", 1, func(ctx context.Context, _ struct{}) (PackageListResult, error) {
		pkgs, err := pkgmgr.SecurityUpgradable(ctx)
		return PackageListResult{Packages: pkgs}, err
	})

	// No pkg.install action yet. Installing arbitrary packages is a far
	// larger grant than reading their status, and nothing in Phase 1 needs
	// it. Phase 2 adds narrowly scoped actions (php.install, and so on) that
	// name their own allowlists instead.
	// Slow on purpose: pkgmgr gives the transaction twenty minutes, and the
	// default action budget is two. Registered as a normal action, the
	// caller was answered "timed out" while dnf was still mid-transaction --
	// the one moment when the honest answer matters most.
	agent.RegisterSlow(r, "pkg.upgrade_all", 1, 25*time.Minute, func(ctx context.Context, in UpgradeRequest) (struct{}, error) {
		return struct{}{}, pkgmgr.UpgradeAll(ctx, in.SecurityOnly)
	})
}
