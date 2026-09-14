package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Plan is a hosting package. A zero limit means unlimited.
type Plan struct {
	ID           int64
	Name         string
	Description  string
	MaxSites     int
	MaxDatabases int
	DiskQuotaMB  int
	DefaultPHP   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// Users is filled by listings, so an operator can see what a package
	// affects before changing it.
	Users int
}

const planCols = `id, name, description, max_sites, max_databases, disk_quota_mb,
	default_php, created_at, updated_at`

func scanPlan(row interface{ Scan(...any) error }) (*Plan, error) {
	var p Plan
	var created, updated string
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.MaxSites, &p.MaxDatabases,
		&p.DiskQuotaMB, &p.DefaultPHP, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if p.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePlan inserts a package.
func (d *DB) CreatePlan(ctx context.Context, p *Plan) (*Plan, error) {
	now := time.Now().UTC()
	res, err := d.ExecContext(ctx,
		`INSERT INTO plans (name, description, max_sites, max_databases, disk_quota_mb,
			default_php, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		p.Name, p.Description, p.MaxSites, p.MaxDatabases, p.DiskQuotaMB,
		p.DefaultPHP, fmtTime(now), fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("db: create plan %q: %w", p.Name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.PlanByID(ctx, id)
}

// PlanByID looks a package up by primary key.
func (d *DB) PlanByID(ctx context.Context, id int64) (*Plan, error) {
	return scanPlan(d.QueryRowContext(ctx, `SELECT `+planCols+` FROM plans WHERE id = ?`, id))
}

// ListPlans returns every package with the number of accounts on it.
func (d *DB) ListPlans(ctx context.Context) ([]*Plan, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+planCols+` FROM plans ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*Plan, 0, 4)
	byID := make(map[int64]*Plan)
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
		byID[p.ID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	counts, err := d.QueryContext(ctx,
		`SELECT plan_id, COUNT(*) FROM users WHERE plan_id IS NOT NULL GROUP BY plan_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = counts.Close() }()
	for counts.Next() {
		var id int64
		var n int
		if err := counts.Scan(&id, &n); err != nil {
			return nil, err
		}
		if p, ok := byID[id]; ok {
			p.Users = n
		}
	}
	return out, counts.Err()
}

// UpdatePlan writes a package's limits.
func (d *DB) UpdatePlan(ctx context.Context, p *Plan) error {
	_, err := d.ExecContext(ctx,
		`UPDATE plans SET name = ?, description = ?, max_sites = ?, max_databases = ?,
			disk_quota_mb = ?, default_php = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Description, p.MaxSites, p.MaxDatabases, p.DiskQuotaMB,
		p.DefaultPHP, fmtTime(time.Now()), p.ID)
	return err
}

// DeletePlan removes a package. Accounts on it are left without one, which
// means unlimited until an operator assigns another.
func (d *DB) DeletePlan(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM plans WHERE id = ?`, id)
	return err
}

// SetUserPlan assigns a package to an account. A nil planID clears it.
func (d *DB) SetUserPlan(ctx context.Context, userID int64, planID *int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE users SET plan_id = ?, updated_at = ? WHERE id = ?`,
		planID, fmtTime(time.Now()), userID)
	return err
}

// UserPlan returns the package assigned to an account, or nil when there is
// none — which the callers treat as unlimited.
func (d *DB) UserPlan(ctx context.Context, userID int64) (*Plan, error) {
	var planID sql.NullInt64
	err := d.QueryRowContext(ctx, `SELECT plan_id FROM users WHERE id = ?`, userID).Scan(&planID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil || !planID.Valid {
		return nil, err
	}
	return d.PlanByID(ctx, planID.Int64)
}

// UserPlanID returns the raw assignment, for rendering.
func (d *DB) UserPlanID(ctx context.Context, userID int64) (*int64, error) {
	var planID sql.NullInt64
	if err := d.QueryRowContext(ctx, `SELECT plan_id FROM users WHERE id = ?`, userID).Scan(&planID); err != nil {
		return nil, err
	}
	if !planID.Valid {
		return nil, nil
	}
	return &planID.Int64, nil
}
