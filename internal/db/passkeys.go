package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Passkey is one registered authenticator.
type Passkey struct {
	ID         string
	UserID     int64
	Label      string
	PublicKey  string
	AAGUID     string
	SignCount  uint32
	Resident   bool
	Transports string
	CreatedAt  time.Time
	LastUsedAt time.Time
	// BackupEligible and BackupState are the flags the credential was
	// registered with. Pointers because nil is a third state that matters:
	// a row written before these existed knows nothing, and answering
	// "false" on its behalf is what stopped every phone passkey working.
	BackupEligible *bool
	BackupState    *bool
}

// PasskeyChallenge is an in-flight ceremony.
type PasskeyChallenge struct {
	ID        string
	UserID    int64
	Purpose   string
	Session   string
	ExpiresAt time.Time
}

const passkeyCols = `id, user_id, label, public_key, aaguid, sign_count,
	resident, transports, created_at, last_used_at, backup_eligible, backup_state`

func scanPasskey(row interface{ Scan(...any) error }) (*Passkey, error) {
	var k Passkey
	var created, used string
	var be, bs sql.NullBool
	err := row.Scan(&k.ID, &k.UserID, &k.Label, &k.PublicKey, &k.AAGUID,
		&k.SignCount, &k.Resident, &k.Transports, &created, &used, &be, &bs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.CreatedAt, _ = parseTime(created)
	k.LastUsedAt, _ = parseTime(used)
	if be.Valid {
		k.BackupEligible = &be.Bool
	}
	if bs.Valid {
		k.BackupState = &bs.Bool
	}
	return &k, nil
}

// CreatePasskey stores a newly registered authenticator.
func (d *DB) CreatePasskey(ctx context.Context, k *Passkey) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO passkeys (id, user_id, label, public_key, aaguid, sign_count,
			resident, transports, created_at, backup_eligible, backup_state)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		k.ID, k.UserID, k.Label, k.PublicKey, k.AAGUID, k.SignCount,
		k.Resident, k.Transports, fmtTime(time.Now()),
		nullBool(k.BackupEligible), nullBool(k.BackupState))
	if err != nil {
		return fmt.Errorf("db: register passkey: %w", err)
	}
	return nil
}

// PasskeysFor lists an account's authenticators, newest first.
func (d *DB) PasskeysFor(ctx context.Context, userID int64) ([]*Passkey, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+passkeyCols+` FROM passkeys WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*Passkey
	for rows.Next() {
		k, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// PasskeyByID returns one credential.
func (d *DB) PasskeyByID(ctx context.Context, id string) (*Passkey, error) {
	return scanPasskey(d.QueryRowContext(ctx,
		`SELECT `+passkeyCols+` FROM passkeys WHERE id = ?`, id))
}

// DeletePasskey removes one of an account's credentials.
//
// Scoped to the owner in the statement rather than checked first, so a
// customer cannot delete somebody else's key by guessing its id.
func (d *DB) DeletePasskey(ctx context.Context, userID int64, id string) error {
	res, err := d.ExecContext(ctx,
		`DELETE FROM passkeys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchPasskey records a successful sign-in and the new signature counter.
func (d *DB) TouchPasskey(ctx context.Context, id string, signCount uint32,
	backupEligible, backupState *bool) error {
	// The backup flags are written on every sign-in, not just the first.
	// BackupState legitimately changes -- a key becomes backed up when
	// somebody turns on syncing -- and a row that never learns the new value
	// is a row that disagrees with the next assertion.
	_, err := d.ExecContext(ctx,
		`UPDATE passkeys SET sign_count = ?, last_used_at = ?,
			backup_eligible = ?, backup_state = ? WHERE id = ?`,
		signCount, fmtTime(time.Now()),
		nullBool(backupEligible), nullBool(backupState), id)
	return err
}

// nullBool turns "we do not know" into a NULL the database can hold.
func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// CreatePasskeyChallenge stores an in-flight ceremony.
func (d *DB) CreatePasskeyChallenge(ctx context.Context, c *PasskeyChallenge) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO passkey_challenges (id, user_id, purpose, session, expires_at)
		 VALUES (?,?,?,?,?)`,
		c.ID, c.UserID, c.Purpose, c.Session, fmtTime(c.ExpiresAt))
	return err
}

// TakePasskeyChallenge returns a challenge and consumes it.
//
// The delete is the check, the way a one-time ticket has to be: two requests
// racing with the same challenge cannot both find it.
func (d *DB) TakePasskeyChallenge(ctx context.Context, id, purpose string) (*PasskeyChallenge, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var c PasskeyChallenge
	var expires string
	err = tx.QueryRowContext(ctx,
		`SELECT id, user_id, purpose, session, expires_at
		   FROM passkey_challenges WHERE id = ? AND purpose = ?`, id, purpose).
		Scan(&c.ID, &c.UserID, &c.Purpose, &c.Session, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM passkey_challenges WHERE id = ?`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	c.ExpiresAt, _ = parseTime(expires)
	if time.Now().After(c.ExpiresAt) {
		return nil, ErrNotFound
	}
	return &c, nil
}

// PurgeExpiredPasskeyChallenges clears ceremonies nobody finished.
func (d *DB) PurgeExpiredPasskeyChallenges(ctx context.Context, now time.Time) error {
	_, err := d.ExecContext(ctx,
		`DELETE FROM passkey_challenges WHERE expires_at < ?`, fmtTime(now))
	return err
}
