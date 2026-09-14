package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BackupDestination is somewhere backups are copied to.
//
// Note what is absent: the credential. It lives in a root-only file beside
// the agent, so a copy of this database is not a copy of anybody's storage
// credentials.
type BackupDestination struct {
	ID        int64
	OwnerID   int64
	OwnerName string
	Name      string
	Kind      string
	Summary   string
	Enabled   bool
	LastOKAt  time.Time
	LastError string
	CreatedAt time.Time
}

const destCols = `d.id, d.owner_id, COALESCE(u.username,''), d.name, d.kind,
	d.summary, d.enabled, d.last_ok_at, d.last_error, d.created_at`

const destJoin = ` FROM backup_destinations d LEFT JOIN users u ON u.id = d.owner_id`

func scanDestination(row interface{ Scan(...any) error }) (*BackupDestination, error) {
	var d BackupDestination
	var lastOK, created string
	err := row.Scan(&d.ID, &d.OwnerID, &d.OwnerName, &d.Name, &d.Kind,
		&d.Summary, &d.Enabled, &lastOK, &d.LastError, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.LastOKAt, _ = parseTime(lastOK)
	d.CreatedAt, _ = parseTime(created)
	return &d, nil
}

// CreateBackupDestination adds one.
func (d *DB) CreateBackupDestination(ctx context.Context, x *BackupDestination) (*BackupDestination, error) {
	res, err := d.ExecContext(ctx,
		`INSERT INTO backup_destinations (owner_id, name, kind, summary, enabled, created_at)
		 VALUES (?,?,?,?,?,?)`,
		x.OwnerID, x.Name, x.Kind, x.Summary, x.Enabled, fmtTime(time.Now()))
	if err != nil {
		return nil, fmt.Errorf("db: create destination: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.BackupDestinationByID(ctx, id)
}

// BackupDestinationByID returns one.
func (d *DB) BackupDestinationByID(ctx context.Context, id int64) (*BackupDestination, error) {
	return scanDestination(d.QueryRowContext(ctx,
		`SELECT `+destCols+destJoin+` WHERE d.id = ?`, id))
}

// UpdateBackupDestination rewrites the parts a person can change.
func (d *DB) UpdateBackupDestination(ctx context.Context, x *BackupDestination) error {
	_, err := d.ExecContext(ctx,
		`UPDATE backup_destinations SET name = ?, summary = ?, enabled = ? WHERE id = ?`,
		x.Name, x.Summary, x.Enabled, x.ID)
	return err
}

// DeleteBackupDestination removes one.
func (d *DB) DeleteBackupDestination(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM backup_destinations WHERE id = ?`, id)
	return err
}

// SetDestinationResult records the outcome of a test or an upload.
func (d *DB) SetDestinationResult(ctx context.Context, id int64, ok bool, errText string) error {
	if ok {
		_, err := d.ExecContext(ctx,
			`UPDATE backup_destinations SET last_ok_at = ?, last_error = '' WHERE id = ?`,
			fmtTime(time.Now()), id)
		return err
	}
	_, err := d.ExecContext(ctx,
		`UPDATE backup_destinations SET last_error = ? WHERE id = ?`, errText, id)
	return err
}

// ListBackupDestinations returns what the scope allows, plus the server-wide
// ones, which apply to everybody.
func (d *DB) ListBackupDestinations(ctx context.Context, scope Scope) ([]*BackupDestination, error) {
	where, args := scope.Where("d.owner_id", "u.parent_id")
	return d.destinations(ctx,
		`WHERE (`+where+`) OR d.owner_id = 0 ORDER BY d.owner_id, d.name`, args...)
}

// DestinationsFor returns where a given account's backups go: its own, plus
// the server's.
func (d *DB) DestinationsFor(ctx context.Context, ownerID int64) ([]*BackupDestination, error) {
	return d.destinations(ctx,
		`WHERE d.enabled = 1 AND (d.owner_id = ? OR d.owner_id = 0) ORDER BY d.id`, ownerID)
}

func (d *DB) destinations(ctx context.Context, clause string, args ...any) ([]*BackupDestination, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+destCols+destJoin+` `+clause, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list destinations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*BackupDestination
	for rows.Next() {
		x, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// BackupCopy is one archive at one destination.
type BackupCopy struct {
	BackupID      int64
	DestinationID int64
	DestName      string
	RemotePath    string
	Bytes         int64
	Status        string
	Error         string
	At            time.Time
}

// RecordBackupCopy notes where an archive went, or why it did not.
func (d *DB) RecordBackupCopy(ctx context.Context, c *BackupCopy) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO backup_copies (backup_id, destination_id, remote_path, bytes, status, error, at)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(backup_id, destination_id) DO UPDATE SET
			remote_path = excluded.remote_path, bytes = excluded.bytes,
			status = excluded.status, error = excluded.error, at = excluded.at`,
		c.BackupID, c.DestinationID, c.RemotePath, c.Bytes, c.Status, c.Error,
		fmtTime(time.Now()))
	return err
}

// BackupCopiesFor returns where one archive was sent.
func (d *DB) BackupCopiesFor(ctx context.Context, backupID int64) ([]*BackupCopy, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT c.backup_id, c.destination_id, COALESCE(d.name,''), c.remote_path,
			c.bytes, c.status, c.error, c.at
		   FROM backup_copies c
		   LEFT JOIN backup_destinations d ON d.id = c.destination_id
		  WHERE c.backup_id = ?`, backupID)
	if err != nil {
		return nil, fmt.Errorf("db: list backup copies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*BackupCopy
	for rows.Next() {
		var c BackupCopy
		var at string
		if err := rows.Scan(&c.BackupID, &c.DestinationID, &c.DestName, &c.RemotePath,
			&c.Bytes, &c.Status, &c.Error, &at); err != nil {
			return nil, err
		}
		c.At, _ = parseTime(at)
		out = append(out, &c)
	}
	return out, rows.Err()
}
