package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Setting keys the panel understands.
const (
	SettingBrandName   = "brand.name"
	SettingBrandLogo   = "brand.logo"    // a data: URI, small
	SettingBrandIcon   = "brand.favicon" // a data: URI, small
	SettingPanelHost   = "panel.hostname"
	SettingPanelPort   = "panel.port"
	SettingIPv6Enabled = "network.ipv6_enabled"
)

// Setting returns a value, or the fallback when the key has never been set.
func (d *DB) Setting(ctx context.Context, key, fallback string) (string, error) {
	var v string
	err := d.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	if err != nil {
		return fallback, err
	}
	return v, nil
}

// Settings returns every stored value.
func (d *DB) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]string, 8)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting stores a value.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, fmtTime(time.Now()))
	return err
}

// PanelHostname is a name the panel will answer to.
type PanelHostname struct {
	Hostname  string
	CertFile  string
	KeyFile   string
	IsPrimary bool
	CreatedAt time.Time
}

// ListPanelHostnames returns the names the panel serves, primary first.
func (d *DB) ListPanelHostnames(ctx context.Context) ([]*PanelHostname, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT hostname, cert_file, key_file, is_primary, created_at
		   FROM panel_hostnames ORDER BY is_primary DESC, hostname`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*PanelHostname
	for rows.Next() {
		var h PanelHostname
		var created string
		if err := rows.Scan(&h.Hostname, &h.CertFile, &h.KeyFile, &h.IsPrimary, &created); err != nil {
			return nil, err
		}
		if created != "" {
			h.CreatedAt, _ = parseTime(created)
		}
		out = append(out, &h)
	}
	return out, rows.Err()
}

// AddPanelHostname records a name the panel may answer on.
func (d *DB) AddPanelHostname(ctx context.Context, h *PanelHostname) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO panel_hostnames (hostname, cert_file, key_file, is_primary, created_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(hostname) DO UPDATE SET
			cert_file = excluded.cert_file, key_file = excluded.key_file,
			is_primary = excluded.is_primary`,
		h.Hostname, h.CertFile, h.KeyFile, h.IsPrimary, fmtTime(time.Now()))
	return err
}

// DeletePanelHostname stops the panel answering on a name.
func (d *DB) DeletePanelHostname(ctx context.Context, hostname string) error {
	_, err := d.ExecContext(ctx, `DELETE FROM panel_hostnames WHERE hostname = ?`, hostname)
	return err
}

// SetPrimaryHostname marks one name as the panel's own, clearing the rest.
func (d *DB) SetPrimaryHostname(ctx context.Context, hostname string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `UPDATE panel_hostnames SET is_primary = 0`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE panel_hostnames SET is_primary = 1 WHERE hostname = ?`, hostname); err != nil {
		return err
	}
	return tx.Commit()
}

// NotificationTarget is where an account's alerts go.
type NotificationTarget struct {
	UserID         int64
	TelegramToken  string
	TelegramChatID string
	Events         string
	Enabled        bool
}

// NotificationTargetFor returns an account's alert settings, or nil.
func (d *DB) NotificationTargetFor(ctx context.Context, userID int64) (*NotificationTarget, error) {
	var t NotificationTarget
	err := d.QueryRowContext(ctx,
		`SELECT user_id, telegram_token, telegram_chat_id, events, enabled
		   FROM notification_targets WHERE user_id = ?`, userID).
		Scan(&t.UserID, &t.TelegramToken, &t.TelegramChatID, &t.Events, &t.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListNotificationTargets returns every configured destination, which is what
// the alert sender works from.
func (d *DB) ListNotificationTargets(ctx context.Context) ([]*NotificationTarget, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT user_id, telegram_token, telegram_chat_id, events, enabled
		   FROM notification_targets WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*NotificationTarget
	for rows.Next() {
		var t NotificationTarget
		if err := rows.Scan(&t.UserID, &t.TelegramToken, &t.TelegramChatID,
			&t.Events, &t.Enabled); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// SaveNotificationTarget stores where an account's alerts go.
func (d *DB) SaveNotificationTarget(ctx context.Context, t *NotificationTarget) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO notification_targets
			(user_id, telegram_token, telegram_chat_id, events, enabled, updated_at)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET
			telegram_token = excluded.telegram_token,
			telegram_chat_id = excluded.telegram_chat_id,
			events = excluded.events, enabled = excluded.enabled,
			updated_at = excluded.updated_at`,
		t.UserID, t.TelegramToken, t.TelegramChatID, t.Events, t.Enabled,
		fmtTime(time.Now()))
	return err
}
