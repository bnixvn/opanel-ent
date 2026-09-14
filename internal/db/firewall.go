package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Firewall rule kinds.
const (
	FirewallPort  = "port"
	FirewallBlock = "block"
	FirewallAllow = "allow"
)

// FirewallRule is one declarative rule.
type FirewallRule struct {
	ID        int64
	Kind      string
	Protocol  string
	PortFrom  int
	PortTo    int
	Address   string
	Comment   string
	Enabled   bool
	Managed   bool
	CreatedAt time.Time
}

const fwCols = `id, kind, protocol, port_from, port_to, address, comment,
	enabled, managed, created_at`

func scanFirewallRule(row interface{ Scan(...any) error }) (*FirewallRule, error) {
	var f FirewallRule
	var created string
	err := row.Scan(&f.ID, &f.Kind, &f.Protocol, &f.PortFrom, &f.PortTo,
		&f.Address, &f.Comment, &f.Enabled, &f.Managed, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.CreatedAt, _ = parseTime(created)
	return &f, nil
}

// ListFirewallRules returns every rule, ports first so a reader sees what is
// open before what is refused.
func (d *DB) ListFirewallRules(ctx context.Context) ([]*FirewallRule, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+fwCols+` FROM firewall_rules
		  ORDER BY CASE kind WHEN 'port' THEN 0 WHEN 'allow' THEN 1 ELSE 2 END,
		           port_from, address, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*FirewallRule, 0, 16)
	for rows.Next() {
		f, err := scanFirewallRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FirewallRuleByID looks one up.
func (d *DB) FirewallRuleByID(ctx context.Context, id int64) (*FirewallRule, error) {
	return scanFirewallRule(d.QueryRowContext(ctx, `SELECT `+fwCols+` FROM firewall_rules WHERE id = ?`, id))
}

// CreateFirewallRule adds a rule.
func (d *DB) CreateFirewallRule(ctx context.Context, f *FirewallRule) (*FirewallRule, error) {
	res, err := d.ExecContext(ctx,
		`INSERT INTO firewall_rules
			(kind, protocol, port_from, port_to, address, comment, enabled, managed, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		f.Kind, f.Protocol, f.PortFrom, f.PortTo, f.Address, f.Comment,
		f.Enabled, f.Managed, fmtTime(time.Now()))
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.FirewallRuleByID(ctx, id)
}

// SetFirewallRuleEnabled turns a rule on or off without losing it.
func (d *DB) SetFirewallRuleEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := d.ExecContext(ctx, `UPDATE firewall_rules SET enabled = ? WHERE id = ?`, enabled, id)
	return err
}

// DeleteFirewallRule removes a rule.
func (d *DB) DeleteFirewallRule(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM firewall_rules WHERE id = ?`, id)
	return err
}

// FirewallSource is a blocklist fetched from a URL.
type FirewallSource struct {
	ID            int64
	URL           string
	Description   string
	Enabled       bool
	IntervalHours int
	LastFetchAt   time.Time
	LastError     string
	EntryCount    int
}

const fwSourceCols = `id, url, description, enabled, interval_hours,
	last_fetch_at, last_error, entry_count`

func scanFirewallSource(row interface{ Scan(...any) error }) (*FirewallSource, error) {
	var s FirewallSource
	var last string
	err := row.Scan(&s.ID, &s.URL, &s.Description, &s.Enabled, &s.IntervalHours,
		&last, &s.LastError, &s.EntryCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if last != "" {
		s.LastFetchAt, _ = parseTime(last)
	}
	return &s, nil
}

// ListFirewallSources returns every blocklist source.
func (d *DB) ListFirewallSources(ctx context.Context) ([]*FirewallSource, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+fwSourceCols+` FROM firewall_sources ORDER BY url`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*FirewallSource, 0, 4)
	for rows.Next() {
		s, err := scanFirewallSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FirewallSourceByID looks one up.
func (d *DB) FirewallSourceByID(ctx context.Context, id int64) (*FirewallSource, error) {
	return scanFirewallSource(d.QueryRowContext(ctx,
		`SELECT `+fwSourceCols+` FROM firewall_sources WHERE id = ?`, id))
}

// CreateFirewallSource adds a blocklist URL.
func (d *DB) CreateFirewallSource(ctx context.Context, s *FirewallSource) (*FirewallSource, error) {
	res, err := d.ExecContext(ctx,
		`INSERT INTO firewall_sources (url, description, enabled, interval_hours, created_at)
		 VALUES (?,?,?,?,?)`,
		s.URL, s.Description, s.Enabled, s.IntervalHours, fmtTime(time.Now()))
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.FirewallSourceByID(ctx, id)
}

// DeleteFirewallSource removes a blocklist and everything it contributed.
func (d *DB) DeleteFirewallSource(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM firewall_sources WHERE id = ?`, id)
	return err
}

// ReplaceSourceEntries swaps a source's addresses for a freshly fetched set.
//
// In one transaction: a partly-replaced blocklist would either drop
// protection or double-count, and the window for that is exactly when the
// ruleset might be rebuilt.
func (d *DB) ReplaceSourceEntries(ctx context.Context, sourceID int64, addresses []string, errMsg string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if errMsg == "" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM firewall_source_entries WHERE source_id = ?`, sourceID); err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx,
			`INSERT OR IGNORE INTO firewall_source_entries (source_id, address) VALUES (?,?)`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for _, a := range addresses {
			if _, err := stmt.ExecContext(ctx, sourceID, a); err != nil {
				return err
			}
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE firewall_sources SET last_fetch_at = ?, last_error = ?, entry_count = ?
		  WHERE id = ?`,
		fmtTime(time.Now()), errMsg, len(addresses), sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// BlocklistAddresses returns every address the enabled sources contributed.
func (d *DB) BlocklistAddresses(ctx context.Context) ([]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT DISTINCT e.address FROM firewall_source_entries e
		   JOIN firewall_sources s ON s.id = e.source_id
		  WHERE s.enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
