package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SFTPAccount is an extra upload credential belonging to one hosting account.
type SFTPAccount struct {
	ID        int64
	OwnerID   int64
	Owner     string
	Username  string
	Note      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const sftpCols = `s.id, s.owner_id, COALESCE(u.username,''), s.username, s.note,
	s.created_at, s.updated_at`

const sftpJoin = ` FROM sftp_accounts s LEFT JOIN users u ON u.id = s.owner_id`

func scanSFTPAccount(row interface{ Scan(...any) error }) (*SFTPAccount, error) {
	var a SFTPAccount
	var created, updated string
	err := row.Scan(&a.ID, &a.OwnerID, &a.Owner, &a.Username, &a.Note, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt, _ = parseTime(created)
	a.UpdatedAt, _ = parseTime(updated)
	return &a, nil
}

// CreateSFTPAccount records a credential the agent has already made.
func (d *DB) CreateSFTPAccount(ctx context.Context, a *SFTPAccount) (*SFTPAccount, error) {
	now := fmtTime(time.Now())
	res, err := d.ExecContext(ctx,
		`INSERT INTO sftp_accounts (owner_id, username, note, created_at, updated_at)
		 VALUES (?,?,?,?,?)`,
		a.OwnerID, a.Username, a.Note, now, now)
	if err != nil {
		return nil, fmt.Errorf("db: create sftp account: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.SFTPAccountByID(ctx, id)
}

// SFTPAccountByID returns one credential.
func (d *DB) SFTPAccountByID(ctx context.Context, id int64) (*SFTPAccount, error) {
	return scanSFTPAccount(d.QueryRowContext(ctx,
		`SELECT `+sftpCols+sftpJoin+` WHERE s.id = ?`, id))
}

// SFTPAccountByName returns one credential by its Linux account name.
func (d *DB) SFTPAccountByName(ctx context.Context, username string) (*SFTPAccount, error) {
	return scanSFTPAccount(d.QueryRowContext(ctx,
		`SELECT `+sftpCols+sftpJoin+` WHERE s.username = ?`, username))
}

// ListSFTPAccounts returns the credentials belonging to one owner.
func (d *DB) ListSFTPAccounts(ctx context.Context, ownerID int64) ([]*SFTPAccount, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+sftpCols+sftpJoin+` WHERE s.owner_id = ? ORDER BY s.username`, ownerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*SFTPAccount, 0, 4)
	for rows.Next() {
		a, err := scanSFTPAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountSFTPAccounts reports how many an owner already holds.
func (d *DB) CountSFTPAccounts(ctx context.Context, ownerID int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sftp_accounts WHERE owner_id = ?`, ownerID).Scan(&n)
	return n, err
}

// TouchSFTPAccount records that something about a credential changed, which
// for these is only ever its password.
func (d *DB) TouchSFTPAccount(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE sftp_accounts SET updated_at = ? WHERE id = ?`, fmtTime(time.Now()), id)
	return err
}

// DeleteSFTPAccount forgets a credential. Removing the Linux account behind
// it is the caller's job, and has to happen first: a row without an account
// is invisible, while an account without a row still lets somebody in.
func (d *DB) DeleteSFTPAccount(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM sftp_accounts WHERE id = ?`, id)
	return err
}
