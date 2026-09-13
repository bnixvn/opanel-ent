package db

import (
	"context"
	"time"
)

// AppendAudit writes one audit row. Audit is append-only: there is no update
// or delete path anywhere in the codebase.
func (d *DB) AppendAudit(ctx context.Context, e AuditEntry) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	_, err := d.ExecContext(ctx,
		`INSERT INTO audit_log (at, actor_type, actor_id, actor_name, action, target, ok, detail, ip)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		fmtTime(e.At), e.ActorType, e.ActorID, e.ActorName,
		e.Action, e.Target, e.OK, e.Detail, e.IP)
	return err
}

// RecentAudit returns the newest entries, most recent first.
func (d *DB) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := d.QueryContext(ctx,
		`SELECT id, at, actor_type, actor_id, actor_name, action, target, ok, detail, ip
		 FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.ActorType, &e.ActorID, &e.ActorName,
			&e.Action, &e.Target, &e.OK, &e.Detail, &e.IP); err != nil {
			return nil, err
		}
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
