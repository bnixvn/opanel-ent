package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("db: not found")

const userCols = `id, username, email, password_hash, role, linux_uid,
	totp_secret, totp_enabled, suspended, created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var uid sql.NullInt64
	var created, updated string
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Role, &uid,
		&u.TOTPSecret, &u.TOTPEnabled, &u.Suspended, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if uid.Valid {
		u.LinuxUID = &uid.Int64
	}
	if u.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("db: user %d created_at: %w", u.ID, err)
	}
	if u.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("db: user %d updated_at: %w", u.ID, err)
	}
	return &u, nil
}

// CreateUser inserts a user and returns it with its assigned ID.
func (d *DB) CreateUser(ctx context.Context, u *User) (*User, error) {
	now := time.Now().UTC()
	u.CreatedAt, u.UpdatedAt = now, now
	res, err := d.ExecContext(ctx,
		`INSERT INTO users (username, email, password_hash, role, linux_uid,
			totp_secret, totp_enabled, suspended, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		u.Username, u.Email, u.PasswordHash, u.Role, u.LinuxUID,
		u.TOTPSecret, u.TOTPEnabled, u.Suspended, fmtTime(now), fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("db: create user %q: %w", u.Username, err)
	}
	if u.ID, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	return u, nil
}

// UserByUsername looks a user up by their login name.
func (d *DB) UserByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(d.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE username = ?`, username))
}

// UserByID looks a user up by primary key.
func (d *DB) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(d.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// CountUsers reports how many panel users exist. Used by the installer to
// decide whether the first-run admin still needs creating.
func (d *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// SetPasswordHash replaces a user's password hash.
func (d *DB) SetPasswordHash(ctx context.Context, userID int64, hash string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, fmtTime(time.Now()), userID)
	return err
}

// SetTOTP stores or clears a user's TOTP secret and enabled flag.
func (d *DB) SetTOTP(ctx context.Context, userID int64, secret string, enabled bool) error {
	_, err := d.ExecContext(ctx,
		`UPDATE users SET totp_secret = ?, totp_enabled = ?, updated_at = ? WHERE id = ?`,
		secret, enabled, fmtTime(time.Now()), userID)
	return err
}
