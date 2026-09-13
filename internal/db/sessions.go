package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CreateSession stores a session keyed by the hash of its cookie value.
func (d *DB) CreateSession(ctx context.Context, s *Session) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, created_at, expires_at, last_seen_at, ip, user_agent)
		 VALUES (?,?,?,?,?,?,?)`,
		s.ID, s.UserID, fmtTime(s.CreatedAt), fmtTime(s.ExpiresAt),
		fmtTime(s.LastSeenAt), s.IP, s.UserAgent)
	return err
}

// SessionByID returns a session by its hashed id, or ErrNotFound.
func (d *DB) SessionByID(ctx context.Context, id string) (*Session, error) {
	var s Session
	var created, expires, seen string
	err := d.QueryRowContext(ctx,
		`SELECT id, user_id, created_at, expires_at, last_seen_at, ip, user_agent
		 FROM sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.UserID, &created, &expires, &seen, &s.IP, &s.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if s.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if s.ExpiresAt, err = parseTime(expires); err != nil {
		return nil, err
	}
	if s.LastSeenAt, err = parseTime(seen); err != nil {
		return nil, err
	}
	return &s, nil
}

// TouchSession records activity, used to enforce the idle timeout.
func (d *DB) TouchSession(ctx context.Context, id string, at time.Time) error {
	_, err := d.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, fmtTime(at), id)
	return err
}

// DeleteSession removes one session (logout).
func (d *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteUserSessions removes every session for a user. Called on password
// change, suspension and deletion so revocation is immediate.
func (d *DB) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PurgeExpiredSessions drops sessions past their absolute expiry and returns
// how many were removed.
func (d *DB) PurgeExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := d.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, fmtTime(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
