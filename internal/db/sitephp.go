package db

import (
	"context"
	"fmt"
)

// SitePHPSettings returns the php.ini overrides for one site.
func (d *DB) SitePHPSettings(ctx context.Context, siteID int64) (map[string]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT name, value FROM site_php_settings WHERE site_id = ?`, siteID)
	if err != nil {
		return nil, fmt.Errorf("db: php settings for site %d: %w", siteID, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, rows.Err()
}

// AllSitePHPSettings returns every site's overrides at once.
//
// The renderer writes every vhost from the database in one pass, so it needs
// all of them; asking per site would be one query per site on every reload.
func (d *DB) AllSitePHPSettings(ctx context.Context) (map[int64]map[string]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT site_id, name, value FROM site_php_settings`)
	if err != nil {
		return nil, fmt.Errorf("db: php settings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[int64]map[string]string{}
	for rows.Next() {
		var id int64
		var name, value string
		if err := rows.Scan(&id, &name, &value); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		out[id][name] = value
	}
	return out, rows.Err()
}

// ReplaceSitePHPSettings makes the stored set exactly what was passed.
//
// A replace rather than an upsert per directive: the form submits the whole
// set, and a directive the customer cleared has to disappear rather than
// linger at its old value.
func (d *DB) ReplaceSitePHPSettings(ctx context.Context, siteID int64, settings map[string]string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM site_php_settings WHERE site_id = ?`, siteID); err != nil {
		return fmt.Errorf("db: clear php settings for site %d: %w", siteID, err)
	}
	for name, value := range settings {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO site_php_settings (site_id, name, value) VALUES (?,?,?)`,
			siteID, name, value); err != nil {
			return fmt.Errorf("db: set php %s for site %d: %w", name, siteID, err)
		}
	}
	return tx.Commit()
}

// PHPVersionSettings returns the ini settings shared by every website on one
// PHP version.
func (d *DB) PHPVersionSettings(ctx context.Context, version string) (map[string]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT name, value FROM php_version_settings WHERE version = ?`, version)
	if err != nil {
		return nil, fmt.Errorf("db: php settings for %s: %w", version, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		out[name] = value
	}
	return out, rows.Err()
}

// ReplacePHPVersionSettings makes the stored set exactly what was passed.
func (d *DB) ReplacePHPVersionSettings(ctx context.Context, version string, settings map[string]string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM php_version_settings WHERE version = ?`, version); err != nil {
		return fmt.Errorf("db: clear php settings for %s: %w", version, err)
	}
	for name, value := range settings {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO php_version_settings (version, name, value) VALUES (?,?,?)`,
			version, name, value); err != nil {
			return fmt.Errorf("db: set php %s for %s: %w", name, version, err)
		}
	}
	return tx.Commit()
}

// SiteWithPHPOverrides is a website that sets php.ini values of its own.
type SiteWithPHPOverrides struct {
	SiteID     int64
	Domain     string
	PHPVersion string
	OwnerID    int64
	Values     map[string]string
}

// SitesWithPHPOverrides lists every website that overrides its version's
// settings.
//
// The panel needs this to show them. An override that nothing displays is
// the worst kind: the version settings page says one thing, the website does
// another, and there is no screen that explains the difference.
func (d *DB) SitesWithPHPOverrides(ctx context.Context) ([]*SiteWithPHPOverrides, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT p.site_id, s.domain, s.php_version, s.owner_id, p.name, p.value
		   FROM site_php_settings p
		   JOIN sites s ON s.id = p.site_id
		  ORDER BY s.domain, p.name`)
	if err != nil {
		return nil, fmt.Errorf("db: php overrides: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*SiteWithPHPOverrides
	byID := map[int64]*SiteWithPHPOverrides{}
	for rows.Next() {
		var id, ownerID int64
		var domain, version, name, value string
		if err := rows.Scan(&id, &domain, &version, &ownerID, &name, &value); err != nil {
			return nil, err
		}
		site := byID[id]
		if site == nil {
			site = &SiteWithPHPOverrides{
				SiteID: id, Domain: domain, PHPVersion: version,
				OwnerID: ownerID, Values: map[string]string{},
			}
			byID[id] = site
			out = append(out, site)
		}
		site.Values[name] = value
	}
	return out, rows.Err()
}
