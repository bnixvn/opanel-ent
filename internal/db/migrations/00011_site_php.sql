-- +goose Up

-- Per-site php.ini overrides.
--
-- A row per directive rather than a blob, so a directive that is dropped
-- from the whitelist stops being rendered the moment the code changes, with
-- no migration and no half-understood JSON to rewrite.
--
-- Values are validated against internal/phpini before they arrive here. The
-- table is not the guard -- it cannot be, since the acceptable range for a
-- directive is a property of the directive, not of SQL -- but the foreign
-- key means a deleted site takes its settings with it.
CREATE TABLE site_php_settings (
    site_id INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    name    TEXT    NOT NULL,
    value   TEXT    NOT NULL,
    PRIMARY KEY (site_id, name)
);

-- +goose Down
DROP TABLE site_php_settings;
