package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// CreateAPIToken stores a token record. The caller keeps the plaintext; only
// its hash reaches the database.
func (d *DB) CreateAPIToken(ctx context.Context, t *APIToken) (*APIToken, error) {
	t.CreatedAt = time.Now().UTC()
	res, err := d.ExecContext(ctx,
		`INSERT INTO api_tokens (name, user_id, prefix, token_hash, scopes, created_at, expires_at)
		 VALUES (?,?,?,?,?,?,?)`,
		t.Name, t.UserID, t.Prefix, t.TokenHash, strings.Join(t.Scopes, " "),
		fmtTime(t.CreatedAt), fmtTimePtr(t.ExpiresAt))
	if err != nil {
		return nil, err
	}
	if t.ID, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	return t, nil
}

// APITokenByPrefix looks a token up by its public prefix. The caller must
// still compare the secret hash; the prefix alone proves nothing.
func (d *DB) APITokenByPrefix(ctx context.Context, prefix string) (*APIToken, error) {
	var t APIToken
	var scopes, created string
	var expires, lastUsed, revoked sql.NullString
	err := d.QueryRowContext(ctx,
		`SELECT id, name, user_id, prefix, token_hash, scopes, created_at,
		        expires_at, last_used_at, revoked_at
		 FROM api_tokens WHERE prefix = ?`, prefix,
	).Scan(&t.ID, &t.Name, &t.UserID, &t.Prefix, &t.TokenHash, &scopes, &created,
		&expires, &lastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if scopes != "" {
		t.Scopes = strings.Fields(scopes)
	}
	if t.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	for _, p := range []struct {
		src sql.NullString
		dst **time.Time
	}{{expires, &t.ExpiresAt}, {lastUsed, &t.LastUsedAt}, {revoked, &t.RevokedAt}} {
		if !p.src.Valid {
			continue
		}
		v, err := parseTime(p.src.String)
		if err != nil {
			return nil, err
		}
		*p.dst = &v
	}
	return &t, nil
}

// TouchAPIToken records last use. Best-effort: a failure here must not fail
// the request that triggered it.
func (d *DB) TouchAPIToken(ctx context.Context, id int64, at time.Time) error {
	_, err := d.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, fmtTime(at), id)
	return err
}

// RevokeAPIToken marks a token unusable without deleting its audit history.
func (d *DB) RevokeAPIToken(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		fmtTime(time.Now()), id)
	return err
}
