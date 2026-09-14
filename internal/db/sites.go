package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Site is a website row. See migration 00002 for why this table is the sole
// source of truth for webserver configuration.
type Site struct {
	ID           int64
	Domain       string
	OwnerID      int64
	AppType      string
	PHPVersion   string
	DocumentRoot string
	Aliases      []string
	RewriteMode  string
	SSLEnabled   bool
	CertFile     string
	KeyFile      string
	CertExpires  time.Time
	ForceHTTPS   bool
	WAFEnabled   bool
	Suspended    bool
	CreatedAt    time.Time
	UpdatedAt    time.Time

	// OwnerUsername and OwnerParentID are filled by queries that join users.
	// Neither is stored on the site row.
	OwnerUsername string
	OwnerParentID int64
}

const siteCols = `s.id, s.domain, s.owner_id, s.app_type, s.php_version, s.document_root,
	s.aliases, s.rewrite_mode, s.ssl_enabled, s.cert_file, s.key_file,
	s.cert_expires_at, s.force_https, s.waf_enabled, s.suspended,
	s.created_at, s.updated_at, u.username, COALESCE(u.parent_id, 0)`

func scanSite(row interface{ Scan(...any) error }) (*Site, error) {
	var s Site
	var aliases, created, updated, certExpires string
	err := row.Scan(&s.ID, &s.Domain, &s.OwnerID, &s.AppType, &s.PHPVersion, &s.DocumentRoot,
		&aliases, &s.RewriteMode, &s.SSLEnabled, &s.CertFile, &s.KeyFile,
		&certExpires, &s.ForceHTTPS, &s.WAFEnabled, &s.Suspended,
		&created, &updated, &s.OwnerUsername, &s.OwnerParentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.Aliases = strings.Fields(aliases)
	if s.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("db: site %d created_at: %w", s.ID, err)
	}
	if s.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("db: site %d updated_at: %w", s.ID, err)
	}
	// Empty until a certificate is issued, which is the normal state for a
	// freshly created site.
	if certExpires != "" {
		if s.CertExpires, err = parseTime(certExpires); err != nil {
			return nil, fmt.Errorf("db: site %d cert_expires_at: %w", s.ID, err)
		}
	}
	return &s, nil
}

const siteJoin = ` FROM sites s JOIN users u ON u.id = s.owner_id`

// CreateSite inserts a site and returns it as stored.
func (d *DB) CreateSite(ctx context.Context, s *Site) (*Site, error) {
	now := time.Now().UTC()
	res, err := d.ExecContext(ctx,
		`INSERT INTO sites (domain, owner_id, app_type, php_version, document_root,
			aliases, rewrite_mode, ssl_enabled, waf_enabled, suspended, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.Domain, s.OwnerID, s.AppType, s.PHPVersion, s.DocumentRoot,
		strings.Join(s.Aliases, " "), s.RewriteMode, s.SSLEnabled, s.WAFEnabled,
		s.Suspended, fmtTime(now), fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("db: create site %q: %w", s.Domain, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.SiteByID(ctx, id)
}

// SiteByID looks a site up by primary key.
func (d *DB) SiteByID(ctx context.Context, id int64) (*Site, error) {
	return scanSite(d.QueryRowContext(ctx, `SELECT `+siteCols+siteJoin+` WHERE s.id = ?`, id))
}

// SiteByDomain looks a site up by its primary domain.
func (d *DB) SiteByDomain(ctx context.Context, domain string) (*Site, error) {
	return scanSite(d.QueryRowContext(ctx, `SELECT `+siteCols+siteJoin+` WHERE s.domain = ?`, domain))
}

// ListSites returns sites ordered by domain, restricted to what the scope
// allows.
func (d *DB) ListSites(ctx context.Context, scope Scope) ([]*Site, error) {
	where, args := scope.Where("s.owner_id", "u.parent_id")
	q := `SELECT ` + siteCols + siteJoin + ` WHERE ` + where + ` ORDER BY s.domain`

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*Site, 0, 8)
	for rows.Next() {
		s, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateSite writes the mutable fields of a site.
func (d *DB) UpdateSite(ctx context.Context, s *Site) error {
	_, err := d.ExecContext(ctx,
		`UPDATE sites SET app_type = ?, php_version = ?, document_root = ?, aliases = ?,
			rewrite_mode = ?, ssl_enabled = ?, cert_file = ?, key_file = ?,
			cert_expires_at = ?, force_https = ?, waf_enabled = ?, suspended = ?, updated_at = ?
		 WHERE id = ?`,
		s.AppType, s.PHPVersion, s.DocumentRoot, strings.Join(s.Aliases, " "),
		s.RewriteMode, s.SSLEnabled, s.CertFile, s.KeyFile,
		certExpiryValue(s.CertExpires), s.ForceHTTPS,
		s.WAFEnabled, s.Suspended, fmtTime(time.Now()), s.ID)
	return err
}

// certExpiryValue renders a zero time as the empty string, so "no certificate"
// and "expires at the zero instant" cannot be confused.
func certExpiryValue(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmtTime(t)
}

// SitesNeedingRenewal returns SSL sites whose certificate expires before the
// given moment, which is what the renewal sweep works from.
func (d *DB) SitesNeedingRenewal(ctx context.Context, before time.Time) ([]*Site, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT `+siteCols+siteJoin+
			` WHERE s.ssl_enabled = 1 AND s.cert_expires_at <> '' AND s.cert_expires_at < ?
			  ORDER BY s.cert_expires_at`, fmtTime(before))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Site
	for rows.Next() {
		x, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// DeleteSite removes a site row. Files and webserver config are the caller's
// responsibility; this only drops the record.
func (d *DB) DeleteSite(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM sites WHERE id = ?`, id)
	return err
}

// CountSitesByOwner reports how many sites a user owns, for plan limits.
func (d *DB) CountSitesByOwner(ctx context.Context, ownerID int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM sites WHERE owner_id = ?`, ownerID).Scan(&n)
	return n, err
}

// SetUserLinuxAccount records the provisioned Linux account for a panel user.
func (d *DB) SetUserLinuxAccount(ctx context.Context, userID, uid int64, home string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE users SET linux_uid = ?, linux_home = ?, updated_at = ? WHERE id = ?`,
		uid, home, fmtTime(time.Now()), userID)
	return err
}

// UserLinuxHome returns the recorded home directory for a panel user.
func (d *DB) UserLinuxHome(ctx context.Context, userID int64) (string, error) {
	var home string
	err := d.QueryRowContext(ctx, `SELECT linux_home FROM users WHERE id = ?`, userID).Scan(&home)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return home, err
}

// UpdateSiteOwner records a site's new owner and where its files now are.
//
// Separate from UpdateSite because it is the one change that also moves
// bytes on disk: the caller has already relocated the directory, and this
// records it. Keeping it apart makes the pairing obvious at every call site.
func (d *DB) UpdateSiteOwner(ctx context.Context, siteID, ownerID int64, documentRoot string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE sites SET owner_id = ?, document_root = ?, updated_at = ? WHERE id = ?`,
		ownerID, documentRoot, fmtTime(time.Now()), siteID)
	return err
}
