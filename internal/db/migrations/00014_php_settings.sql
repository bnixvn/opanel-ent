-- +goose Up

-- php.ini settings that apply to every website on one PHP version.
--
-- This is where the settings people actually change belong: a memory limit
-- or an upload size is a property of the hosting, not of one website, and
-- setting it once per version is the difference between one decision and one
-- decision per site.
--
-- Per-site overrides still exist, in site_php_settings, and still win: they
-- are written into the vhost as php_admin_value, which the interpreter
-- applies after reading its ini files.
CREATE TABLE php_version_settings (
    -- "8.4", the same string a website stores.
    version TEXT NOT NULL,
    name    TEXT NOT NULL,
    value   TEXT NOT NULL,
    PRIMARY KEY (version, name)
);

-- +goose Down
DROP TABLE php_version_settings;
