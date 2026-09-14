-- +goose Up

-- Per-site TLS. The paths are stored rather than derived so a certificate
-- supplied by hand keeps working, and the expiry is stored so renewal does not
-- need to parse every certificate on the disk on every sweep.
ALTER TABLE sites ADD COLUMN cert_file TEXT NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN key_file TEXT NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN cert_expires_at TEXT NOT NULL DEFAULT '';

-- Redirecting plain HTTP is a per-site choice: a site behind a CDN that
-- terminates TLS elsewhere must not be forced.
ALTER TABLE sites ADD COLUMN force_https INTEGER NOT NULL DEFAULT 0 CHECK (force_https IN (0,1));

-- +goose Down
