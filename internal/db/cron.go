package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CronJob is one scheduled command.
type CronJob struct {
	ID        int64
	UserID    int64
	Username  string
	Schedule  string
	Command   string
	Comment   string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

const cronCols = `c.id, c.user_id, COALESCE(u.username,''), c.schedule, c.command,
	c.comment, c.enabled, c.created_at, c.updated_at`

const cronJoin = ` FROM cron_jobs c LEFT JOIN users u ON u.id = c.user_id`

func scanCronJob(row interface{ Scan(...any) error }) (*CronJob, error) {
	var j CronJob
	var created, updated string
	err := row.Scan(&j.ID, &j.UserID, &j.Username, &j.Schedule, &j.Command,
		&j.Comment, &j.Enabled, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.CreatedAt, _ = parseTime(created)
	j.UpdatedAt, _ = parseTime(updated)
	return &j, nil
}

// CreateCronJob adds a job.
func (d *DB) CreateCronJob(ctx context.Context, j *CronJob) (*CronJob, error) {
	now := fmtTime(time.Now())
	res, err := d.ExecContext(ctx,
		`INSERT INTO cron_jobs (user_id, schedule, command, comment, enabled, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?)`,
		j.UserID, j.Schedule, j.Command, j.Comment, j.Enabled, now, now)
	if err != nil {
		return nil, fmt.Errorf("db: create cron job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.CronJobByID(ctx, id)
}

// CronJobByID returns one job.
func (d *DB) CronJobByID(ctx context.Context, id int64) (*CronJob, error) {
	return scanCronJob(d.QueryRowContext(ctx,
		`SELECT `+cronCols+cronJoin+` WHERE c.id = ?`, id))
}

// UpdateCronJob rewrites a job.
func (d *DB) UpdateCronJob(ctx context.Context, j *CronJob) error {
	_, err := d.ExecContext(ctx,
		`UPDATE cron_jobs SET schedule = ?, command = ?, comment = ?, enabled = ?,
			updated_at = ? WHERE id = ?`,
		j.Schedule, j.Command, j.Comment, j.Enabled, fmtTime(time.Now()), j.ID)
	return err
}

// DeleteCronJob removes a job.
func (d *DB) DeleteCronJob(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM cron_jobs WHERE id = ?`, id)
	return err
}

// CronJobsFor returns one account's jobs, oldest first so the rendered file
// is stable.
func (d *DB) CronJobsFor(ctx context.Context, userID int64) ([]*CronJob, error) {
	return d.cronJobs(ctx, `WHERE c.user_id = ? ORDER BY c.id`, userID)
}

// ListCronJobs returns every job the scope allows.
func (d *DB) ListCronJobs(ctx context.Context, scope Scope) ([]*CronJob, error) {
	where, args := scope.Where("c.user_id", "u.parent_id")
	return d.cronJobs(ctx, `WHERE `+where+` ORDER BY u.username, c.id`, args...)
}

func (d *DB) cronJobs(ctx context.Context, clause string, args ...any) ([]*CronJob, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+cronCols+cronJoin+` `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list cron jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*CronJob
	for rows.Next() {
		j, err := scanCronJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// CronImported reports whether an existing crontab has already been taken
// over for this account.
func (d *DB) CronImported(ctx context.Context, userID int64) (bool, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cron_imported WHERE user_id = ?`, userID).Scan(&n)
	return n > 0, err
}

// MarkCronImported records that the import has happened, so it happens once.
func (d *DB) MarkCronImported(ctx context.Context, userID int64) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO cron_imported (user_id, at) VALUES (?,?)
		 ON CONFLICT(user_id) DO NOTHING`,
		userID, fmtTime(time.Now()))
	return err
}
