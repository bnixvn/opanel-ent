package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Scope says which accounts' rows a caller may see.
//
// It replaces the earlier "ownerID, where 0 means everything" convention,
// which could express "mine" and "all" but not "mine and my customers'" --
// the one a reseller actually needs. Getting this wrong shows one customer
// another's data, so it is one type used by every listing rather than a
// condition rebuilt in each handler.
type Scope struct {
	// Everything is set for an administrator.
	Everything bool
	// SelfID is the caller's own account.
	SelfID int64
	// Children includes rows owned by accounts whose parent is SelfID.
	Children bool
}

// ScopeAll returns the administrator's scope.
func ScopeAll() Scope { return Scope{Everything: true} }

// ScopeSelf restricts a listing to one account.
func ScopeSelf(id int64) Scope { return Scope{SelfID: id} }

// ScopeSubtree covers a reseller: their own rows and their customers'.
func ScopeSubtree(id int64) Scope { return Scope{SelfID: id, Children: true} }

// Where renders the scope as a SQL condition.
//
// ownerCol is the column holding the owning user id, and parentCol the
// parent_id of the joined users row. A query that cannot join users must not
// use a subtree scope; Owners() is there for that case.
func (s Scope) Where(ownerCol, parentCol string) (string, []any) {
	if s.Everything {
		return "1 = 1", nil
	}
	if !s.Children {
		return ownerCol + " = ?", []any{s.SelfID}
	}
	return "(" + ownerCol + " = ? OR " + parentCol + " = ?)", []any{s.SelfID, s.SelfID}
}

// And appends the scope to an existing WHERE clause.
func (s Scope) And(query, ownerCol, parentCol string, args []any) (string, []any) {
	cond, condArgs := s.Where(ownerCol, parentCol)
	sep := " WHERE "
	if strings.Contains(strings.ToUpper(query), " WHERE ") {
		sep = " AND "
	}
	return query + sep + cond, append(args, condArgs...)
}

// Covers reports whether the scope includes a given owner. parentOf is that
// owner's parent_id, zero when they have none.
func (s Scope) Covers(ownerID, parentOf int64) bool {
	switch {
	case s.Everything:
		return true
	case ownerID == s.SelfID:
		return true
	case s.Children && parentOf == s.SelfID:
		return true
	default:
		return false
	}
}

// ResellerLimits is what a reseller may hand out in total. A zero field means
// no limit on that dimension.
type ResellerLimits struct {
	UserID         int64
	MaxAccounts    int
	MaxSites       int
	MaxDatabases   int
	DiskQuotaMB    int
	BandwidthGB    int
	CanCreatePlans bool
	AllowOversell  bool
	UpdatedAt      time.Time
}

const resellerCols = `user_id, max_accounts, max_sites, max_databases,
	disk_quota_mb, bandwidth_gb, can_create_plans, allow_oversell, updated_at`

func scanResellerLimits(row interface{ Scan(...any) error }) (*ResellerLimits, error) {
	var l ResellerLimits
	var updated string
	err := row.Scan(&l.UserID, &l.MaxAccounts, &l.MaxSites, &l.MaxDatabases,
		&l.DiskQuotaMB, &l.BandwidthGB, &l.CanCreatePlans, &l.AllowOversell, &updated)
	if err != nil {
		return nil, err
	}
	if updated != "" {
		l.UpdatedAt, _ = parseTime(updated)
	}
	return &l, nil
}

// ResellerLimitsFor returns a reseller's allowance.
//
// A reseller with no row is unlimited rather than blocked: the row is created
// when an administrator sets a limit, and until then the account behaves the
// way it did before this table existed.
func (d *DB) ResellerLimitsFor(ctx context.Context, userID int64) (*ResellerLimits, error) {
	l, err := scanResellerLimits(d.QueryRowContext(ctx,
		`SELECT `+resellerCols+` FROM reseller_limits WHERE user_id = ?`, userID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &ResellerLimits{UserID: userID, CanCreatePlans: true}, nil
		}
		return nil, err
	}
	return l, nil
}

// SaveResellerLimits inserts or replaces an allowance.
func (d *DB) SaveResellerLimits(ctx context.Context, l *ResellerLimits) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO reseller_limits
			(user_id, max_accounts, max_sites, max_databases, disk_quota_mb,
			 bandwidth_gb, can_create_plans, allow_oversell, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET
			max_accounts = excluded.max_accounts,
			max_sites = excluded.max_sites,
			max_databases = excluded.max_databases,
			disk_quota_mb = excluded.disk_quota_mb,
			bandwidth_gb = excluded.bandwidth_gb,
			can_create_plans = excluded.can_create_plans,
			allow_oversell = excluded.allow_oversell,
			updated_at = excluded.updated_at`,
		l.UserID, l.MaxAccounts, l.MaxSites, l.MaxDatabases, l.DiskQuotaMB,
		l.BandwidthGB, l.CanCreatePlans, l.AllowOversell,
		fmtTime(time.Now()), fmtTime(time.Now()))
	return err
}

// ResellerAllocation is the total already handed out to a reseller's
// customers, which is what their limits are checked against.
type ResellerAllocation struct {
	Accounts    int
	Sites       int
	Databases   int
	DiskQuotaMB int
}

// AllocationFor sums what a reseller has already committed.
//
// Disk is summed from the packages assigned to their customers, not from
// disk in use: a customer sitting at 1% of a 10 GB package has still had
// 10 GB promised to them, and that is the number that must not be oversold.
func (d *DB) AllocationFor(ctx context.Context, resellerID int64) (ResellerAllocation, error) {
	var a ResellerAllocation
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(COALESCE(p.disk_quota_mb, 0)), 0)
		   FROM users u
		   LEFT JOIN plans p ON p.id = u.plan_id
		  WHERE u.parent_id = ?`, resellerID).Scan(&a.Accounts, &a.DiskQuotaMB)
	if err != nil {
		return a, err
	}
	err = d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sites s JOIN users u ON u.id = s.owner_id
		  WHERE u.parent_id = ?`, resellerID).Scan(&a.Sites)
	if err != nil {
		return a, err
	}
	err = d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM db_databases dd JOIN users u ON u.id = dd.owner_id
		  WHERE u.parent_id = ?`, resellerID).Scan(&a.Databases)
	return a, err
}

// SetUserParent moves an account under a reseller, or to the server itself
// when parentID is zero.
func (d *DB) SetUserParent(ctx context.Context, userID, parentID int64) error {
	if parentID == 0 {
		_, err := d.ExecContext(ctx,
			`UPDATE users SET parent_id = NULL, updated_at = ? WHERE id = ?`,
			fmtTime(time.Now()), userID)
		return err
	}
	_, err := d.ExecContext(ctx,
		`UPDATE users SET parent_id = ?, updated_at = ? WHERE id = ?`,
		parentID, fmtTime(time.Now()), userID)
	return err
}
