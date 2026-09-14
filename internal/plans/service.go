// Package plans enforces hosting package limits.
//
// Limits are checked at the moment something is created rather than swept
// periodically: refusing the twenty-first site is understandable, while
// deleting it an hour later is not.
package plans

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/db"
)

// Service checks and reports package usage.
type Service struct {
	db    *db.DB
	agent *agentclient.Client
	log   *slog.Logger
}

// New builds a Service.
func New(database *db.DB, ac *agentclient.Client, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: database, agent: ac, log: log}
}

// ErrLimitReached is returned when a package limit would be exceeded.
var ErrLimitReached = errors.New("plans: package limit reached")

// Usage is what an account currently consumes against its package.
type Usage struct {
	Sites     int   `json:"sites"`
	Databases int   `json:"databases"`
	DiskBytes int64 `json:"disk_bytes"`

	MaxSites     int `json:"max_sites"`
	MaxDatabases int `json:"max_databases"`
	DiskQuotaMB  int `json:"disk_quota_mb"`

	PlanID   *int64 `json:"plan_id,omitempty"`
	PlanName string `json:"plan_name,omitempty"`
}

// CheckSite reports whether the account may create another site.
func (s *Service) CheckSite(ctx context.Context, ownerID int64) error {
	plan, err := s.db.UserPlan(ctx, ownerID)
	if err != nil || plan == nil || plan.MaxSites == 0 {
		return err // no package, or unlimited
	}
	n, err := s.db.CountSitesByOwner(ctx, ownerID)
	if err != nil {
		return err
	}
	if n >= plan.MaxSites {
		return fmt.Errorf("%w: package %q allows %d site(s) and %d already exist",
			ErrLimitReached, plan.Name, plan.MaxSites, n)
	}
	return nil
}

// CheckDatabase reports whether the account may create another database.
func (s *Service) CheckDatabase(ctx context.Context, ownerID int64) error {
	plan, err := s.db.UserPlan(ctx, ownerID)
	if err != nil || plan == nil || plan.MaxDatabases == 0 {
		return err
	}
	n, err := s.db.CountDatabasesByOwner(ctx, ownerID)
	if err != nil {
		return err
	}
	if n >= plan.MaxDatabases {
		return fmt.Errorf("%w: package %q allows %d database(s) and %d already exist",
			ErrLimitReached, plan.Name, plan.MaxDatabases, n)
	}
	return nil
}

// UsageFor gathers what an account consumes. Disk usage is measured on the
// host and a failure there is not fatal — a usage panel missing one figure is
// better than a page that will not load.
func (s *Service) UsageFor(ctx context.Context, u *db.User) (*Usage, error) {
	out := &Usage{}
	var err error
	if out.Sites, err = s.db.CountSitesByOwner(ctx, u.ID); err != nil {
		return nil, err
	}
	if out.Databases, err = s.db.CountDatabasesByOwner(ctx, u.ID); err != nil {
		return nil, err
	}
	if out.PlanID, err = s.db.UserPlanID(ctx, u.ID); err != nil {
		return nil, err
	}
	if plan, err := s.db.UserPlan(ctx, u.ID); err == nil && plan != nil {
		out.PlanName = plan.Name
		out.MaxSites = plan.MaxSites
		out.MaxDatabases = plan.MaxDatabases
		out.DiskQuotaMB = plan.DiskQuotaMB
	}
	if u.LinuxUID != nil && *u.LinuxUID > 0 {
		res, err := agentclient.Call[actions.DiskUsageResult](ctx, s.agent, "quota.usage", 1,
			actions.DiskUsageRequest{Username: u.Username})
		if err != nil {
			s.log.Warn("plans: could not measure disk usage", "user", u.Username, "err", err)
		} else {
			out.DiskBytes = res.Bytes
		}
	}
	return out, nil
}

// OverQuota reports whether an account is above its disk allowance.
//
// Advisory only. Enforcing it by blocking writes would need a real filesystem
// quota, which the target host cannot enable without a reboot; what this
// drives is the warning an operator acts on.
func (u Usage) OverQuota() bool {
	return u.DiskQuotaMB > 0 && u.DiskBytes > int64(u.DiskQuotaMB)*1024*1024
}
