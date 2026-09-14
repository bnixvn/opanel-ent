package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Backup statuses.
const (
	BackupRunning = "running"
	BackupReady   = "ready"
	BackupFailed  = "failed"
)

// Backup kinds.
const (
	BackupManual    = "manual"
	BackupScheduled = "scheduled"
	BackupUploaded  = "uploaded"
)

// Backup is one archive. See migration 00006 for why the row is written
// before the work starts.
type Backup struct {
	ID         int64
	OwnerID    int64
	Filename   string
	Kind       string
	Status     string
	Error      string
	SizeBytes  int64
	FileCount  int64
	Databases  []string
	HasFiles   bool
	SHA256     string
	CreatedAt  time.Time
	FinishedAt time.Time

	// OwnerUsername is filled by queries that join users; it is not stored.
	OwnerUsername string
}

const backupCols = `b.id, b.owner_id, b.filename, b.kind, b.status, b.error,
	b.size_bytes, b.file_count, b.databases, b.has_files, b.sha256,
	b.created_at, b.finished_at, u.username`

const backupJoin = ` FROM backups b JOIN users u ON u.id = b.owner_id`

func scanBackup(row interface{ Scan(...any) error }) (*Backup, error) {
	var b Backup
	var dbs, created, finished string
	err := row.Scan(&b.ID, &b.OwnerID, &b.Filename, &b.Kind, &b.Status, &b.Error,
		&b.SizeBytes, &b.FileCount, &dbs, &b.HasFiles, &b.SHA256,
		&created, &finished, &b.OwnerUsername)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.Databases = strings.Fields(dbs)
	if b.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	// Empty while the backup is still running, which is the normal state for
	// the row a caller sees first.
	if finished != "" {
		if b.FinishedAt, err = parseTime(finished); err != nil {
			return nil, err
		}
	}
	return &b, nil
}

// CreateBackup inserts a row for a backup that is about to start.
func (d *DB) CreateBackup(ctx context.Context, b *Backup) (*Backup, error) {
	res, err := d.ExecContext(ctx,
		`INSERT INTO backups (owner_id, filename, kind, status, databases, has_files, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		b.OwnerID, b.Filename, b.Kind, BackupRunning,
		strings.Join(b.Databases, " "), b.HasFiles, fmtTime(time.Now()))
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.BackupByID(ctx, id)
}

// BackupByID looks a backup up by primary key.
func (d *DB) BackupByID(ctx context.Context, id int64) (*Backup, error) {
	return scanBackup(d.QueryRowContext(ctx, `SELECT `+backupCols+backupJoin+` WHERE b.id = ?`, id))
}

// ListBackups returns backups newest first. An ownerID of 0 means every owner.
func (d *DB) ListBackups(ctx context.Context, ownerID int64) ([]*Backup, error) {
	q := `SELECT ` + backupCols + backupJoin
	var args []any
	if ownerID != 0 {
		q += ` WHERE b.owner_id = ?`
		args = append(args, ownerID)
	}
	q += ` ORDER BY b.created_at DESC, b.id DESC`

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*Backup, 0, 8)
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// FinishBackup records the outcome of a run.
func (d *DB) FinishBackup(ctx context.Context, id int64, size, files int64, sha, errMsg string) error {
	status := BackupReady
	if errMsg != "" {
		status = BackupFailed
	}
	_, err := d.ExecContext(ctx,
		`UPDATE backups SET status = ?, error = ?, size_bytes = ?, file_count = ?,
			sha256 = ?, finished_at = ? WHERE id = ?`,
		status, errMsg, size, files, sha, fmtTime(time.Now()), id)
	return err
}

// DeleteBackup removes the row. The archive on disk is the caller's business.
func (d *DB) DeleteBackup(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id)
	return err
}

// PrunableBackups returns an owner's ready scheduled backups beyond the most
// recent keep, oldest first.
//
// Only scheduled ones: a backup somebody took by hand before a risky change
// is not something a retention rule should quietly remove.
func (d *DB) PrunableBackups(ctx context.Context, ownerID int64, keep int) ([]*Backup, error) {
	if keep < 1 {
		keep = 1
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+backupCols+backupJoin+
			` WHERE b.owner_id = ? AND b.kind = ? AND b.status = ?
			  ORDER BY b.created_at DESC, b.id DESC LIMIT -1 OFFSET ?`,
		ownerID, BackupScheduled, BackupReady, keep)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkStaleBackupsFailed closes out rows left running by a crash or a
// restart. Called at startup: a row stuck at "running" for ever is worse
// than one that admits the run was lost.
func (d *DB) MarkStaleBackupsFailed(ctx context.Context) (int64, error) {
	res, err := d.ExecContext(ctx,
		`UPDATE backups SET status = ?, error = ?, finished_at = ?
		 WHERE status = ?`,
		BackupFailed, "interrupted by a panel restart", fmtTime(time.Now()), BackupRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BackupSchedule is an account's automatic backup setting.
type BackupSchedule struct {
	OwnerID          int64
	Enabled          bool
	Frequency        string // daily | weekly
	Hour             int
	Weekday          int
	Keep             int
	IncludeFiles     bool
	IncludeDatabases bool
	LastRunAt        time.Time
	LastError        string

	OwnerUsername string
}

const scheduleCols = `s.owner_id, s.enabled, s.frequency, s.hour, s.weekday, s.keep,
	s.include_files, s.include_databases, s.last_run_at, s.last_error, u.username`

func scanSchedule(row interface{ Scan(...any) error }) (*BackupSchedule, error) {
	var s BackupSchedule
	var last string
	err := row.Scan(&s.OwnerID, &s.Enabled, &s.Frequency, &s.Hour, &s.Weekday, &s.Keep,
		&s.IncludeFiles, &s.IncludeDatabases, &last, &s.LastError, &s.OwnerUsername)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if last != "" {
		if s.LastRunAt, err = parseTime(last); err != nil {
			return nil, err
		}
	}
	return &s, nil
}

const scheduleJoin = ` FROM backup_schedules s JOIN users u ON u.id = s.owner_id`

// BackupScheduleFor returns an account's schedule, or ErrNotFound.
func (d *DB) BackupScheduleFor(ctx context.Context, ownerID int64) (*BackupSchedule, error) {
	return scanSchedule(d.QueryRowContext(ctx,
		`SELECT `+scheduleCols+scheduleJoin+` WHERE s.owner_id = ?`, ownerID))
}

// ListBackupSchedules returns every schedule, which is what the sweep works
// from.
func (d *DB) ListBackupSchedules(ctx context.Context) ([]*BackupSchedule, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+scheduleCols+scheduleJoin+` ORDER BY u.username`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*BackupSchedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SaveBackupSchedule inserts or replaces an account's schedule, preserving
// the last-run bookkeeping the sweep owns.
func (d *DB) SaveBackupSchedule(ctx context.Context, s *BackupSchedule) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO backup_schedules
			(owner_id, enabled, frequency, hour, weekday, keep, include_files, include_databases)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(owner_id) DO UPDATE SET
			enabled = excluded.enabled, frequency = excluded.frequency,
			hour = excluded.hour, weekday = excluded.weekday, keep = excluded.keep,
			include_files = excluded.include_files,
			include_databases = excluded.include_databases`,
		s.OwnerID, s.Enabled, s.Frequency, s.Hour, s.Weekday, s.Keep,
		s.IncludeFiles, s.IncludeDatabases)
	return err
}

// DeleteBackupSchedule turns automatic backups off for an account.
func (d *DB) DeleteBackupSchedule(ctx context.Context, ownerID int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM backup_schedules WHERE owner_id = ?`, ownerID)
	return err
}

// RecordScheduleRun stamps a schedule after the sweep has acted on it.
func (d *DB) RecordScheduleRun(ctx context.Context, ownerID int64, at time.Time, errMsg string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE backup_schedules SET last_run_at = ?, last_error = ? WHERE owner_id = ?`,
		fmtTime(at), errMsg, ownerID)
	return err
}
