package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SSOTicket is a one-time pass into phpMyAdmin.
type SSOTicket struct {
	TokenHash  string
	UserID     int64
	DBUser     string
	DBPassword string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	UsedAt     time.Time
}

// CreateSSOTicket stores a ticket.
func (d *DB) CreateSSOTicket(ctx context.Context, t *SSOTicket) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO sso_tickets (token_hash, user_id, db_user, db_password, created_at, expires_at)
		 VALUES (?,?,?,?,?,?)`,
		t.TokenHash, t.UserID, t.DBUser, t.DBPassword,
		fmtTime(time.Now()), fmtTime(t.ExpiresAt))
	return err
}

// RedeemSSOTicket returns a ticket and marks it used, in one statement.
//
// The update is the check: two requests racing with the same token cannot
// both find it unused, because only one UPDATE can match the row while
// used_at is still empty.
func (d *DB) RedeemSSOTicket(ctx context.Context, tokenHash string) (*SSOTicket, error) {
	now := time.Now()
	res, err := d.ExecContext(ctx,
		`UPDATE sso_tickets SET used_at = ?
		  WHERE token_hash = ? AND used_at = '' AND expires_at > ?`,
		fmtTime(now), tokenHash, fmtTime(now))
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNotFound
	}

	var t SSOTicket
	var created, expires, used string
	err = d.QueryRowContext(ctx,
		`SELECT token_hash, user_id, db_user, db_password, created_at, expires_at, used_at
		   FROM sso_tickets WHERE token_hash = ?`, tokenHash).
		Scan(&t.TokenHash, &t.UserID, &t.DBUser, &t.DBPassword, &created, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt, _ = parseTime(created)
	t.ExpiresAt, _ = parseTime(expires)
	t.UsedAt, _ = parseTime(used)
	return &t, nil
}

// ExpiredSSOAccounts returns the throwaway MariaDB accounts whose ticket has
// passed, so they can be dropped.
//
// Expiry rather than use: a ticket that was redeemed is still backing a live
// phpMyAdmin session, and dropping the account underneath it would log the
// customer out mid-query.
func (d *DB) ExpiredSSOAccounts(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT db_user FROM sso_tickets WHERE expires_at < ?`, fmtTime(before))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteSSOTickets removes ticket rows for the given accounts.
func (d *DB) DeleteSSOTickets(ctx context.Context, dbUsers []string) error {
	for _, u := range dbUsers {
		if _, err := d.ExecContext(ctx, `DELETE FROM sso_tickets WHERE db_user = ?`, u); err != nil {
			return err
		}
	}
	return nil
}
