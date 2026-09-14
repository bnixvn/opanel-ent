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

	// QuotaEnforced says whether DiskBytes came from a filesystem that is
	// refusing writes past the limit, or from a measurement nothing acts on.
	QuotaEnforced bool `json:"quota_enforced"`
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
		// quota.report answers from the filesystem when project quota is
		// enforced, and falls back to du when it is not. The panel shows
		// which of the two it got, because "12 GB used" means something
		// different when nothing is stopping it becoming 13.
		res, err := agentclient.Call[actions.QuotaReport](ctx, s.agent, "quota.report", 1,
			actions.DiskUsageRequest{Username: u.Username})
		if err != nil {
			s.log.Warn("plans: could not measure disk usage", "user", u.Username, "err", err)
		} else {
			out.DiskBytes = res.UsedBytes
			out.QuotaEnforced = res.Enforced
		}
	}
	return out, nil
}

// ApplyQuota pushes an account's package limit down to the filesystem.
//
// Called whenever the package changes or the account is created. A failure is
// returned rather than logged: an operator who assigned a 5 GB package and
// was told it worked, on a server where it did not, has been lied to.
func (s *Service) ApplyQuota(ctx context.Context, u *db.User) error {
	if u.LinuxUID == nil || *u.LinuxUID == 0 {
		return nil // staff accounts have no home to limit
	}
	limit := 0
	if plan, err := s.db.UserPlan(ctx, u.ID); err == nil && plan != nil {
		limit = plan.DiskQuotaMB
	}
	_, err := agentclient.Call[actions.QuotaReport](ctx, s.agent, "quota.set", 1,
		actions.QuotaSetRequest{Username: u.Username, LimitMB: int64(limit)})
	return err
}

// QuotaStatus reports whether the host is enforcing quotas at all.
func (s *Service) QuotaStatus(ctx context.Context) (actions.QuotaStatus, error) {
	return agentclient.Call[actions.QuotaStatus](ctx, s.agent, "quota.status", 1, struct{}{})
}

// OverQuota reports whether an account is above its disk allowance.
//
// With project quota enforced this should never be true, because the write
// that would have crossed the line failed instead. It stays because a host
// that has not been rebooted into quota enforcement still needs the warning,
// and because an account whose package was lowered below what it already
// holds is over its allowance without having written anything.
func (u Usage) OverQuota() bool {
	return u.DiskQuotaMB > 0 && u.DiskBytes > int64(u.DiskQuotaMB)*1024*1024
}
