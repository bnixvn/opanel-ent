-- +goose Up

-- A website served by the panel.
--
-- Everything needed to regenerate the webserver configuration lives here.
-- Nothing may exist only in a rendered config file: switching between the
-- OpenLiteSpeed and LiteSpeed Enterprise backends means re-rendering every
-- site from this table, and that is only safe if the table is the whole truth.
CREATE TABLE sites (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    domain        TEXT    NOT NULL UNIQUE,
    owner_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    app_type      TEXT    NOT NULL CHECK (app_type IN ('static','php','wordpress')),
    -- Empty for static sites. Stored as "8.4", never as a binary path: the
    -- path is derived by whichever PHP provider is active, so switching from
    -- lsphp to CloudLinux alt-php is a re-render, not a data migration.
    php_version   TEXT    NOT NULL DEFAULT '',
    document_root TEXT    NOT NULL,
    -- Space-separated additional hostnames served by the same vhost.
    aliases       TEXT    NOT NULL DEFAULT '',
    rewrite_mode  TEXT    NOT NULL DEFAULT 'none'
                          CHECK (rewrite_mode IN ('none','front_controller','laravel','codeigniter')),
    ssl_enabled   INTEGER NOT NULL DEFAULT 0 CHECK (ssl_enabled IN (0,1)),
    waf_enabled   INTEGER NOT NULL DEFAULT 0 CHECK (waf_enabled IN (0,1)),
    suspended     INTEGER NOT NULL DEFAULT 0 CHECK (suspended IN (0,1)),
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL
);
CREATE INDEX idx_sites_owner ON sites(owner_id);

-- A static site has no PHP version and a scripted one must have one. Enforced
-- here as well as in Go so a direct database edit cannot produce a site the
-- renderer has no interpreter for.
-- +goose StatementBegin
CREATE TRIGGER sites_php_version_required_insert
BEFORE INSERT ON sites
WHEN (NEW.app_type IN ('php','wordpress') AND NEW.php_version = '')
   OR (NEW.app_type = 'static' AND NEW.php_version <> '')
BEGIN
    SELECT RAISE(ABORT, 'php_version must be set for php/wordpress and empty for static');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER sites_php_version_required_update
BEFORE UPDATE ON sites
WHEN (NEW.app_type IN ('php','wordpress') AND NEW.php_version = '')
   OR (NEW.app_type = 'static' AND NEW.php_version <> '')
BEGIN
    SELECT RAISE(ABORT, 'php_version must be set for php/wordpress and empty for static');
END;
-- +goose StatementEnd

-- The Linux account backing a panel user. users.linux_uid already carries the
-- uid; this records where its home directory landed.
ALTER TABLE users ADD COLUMN linux_home TEXT NOT NULL DEFAULT '';

-- +goose Down
DROP TRIGGER sites_php_version_required_update;
DROP TRIGGER sites_php_version_required_insert;
DROP TABLE sites;
