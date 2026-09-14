-- +goose Up

-- A hosting package: what an end user is allowed to consume.
--
-- Zero means unlimited throughout, which is the convention every panel uses
-- and avoids a nullable column for every limit.
CREATE TABLE plans (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL UNIQUE,
    description   TEXT    NOT NULL DEFAULT '',
    max_sites     INTEGER NOT NULL DEFAULT 0 CHECK (max_sites >= 0),
    max_databases INTEGER NOT NULL DEFAULT 0 CHECK (max_databases >= 0),
    disk_quota_mb INTEGER NOT NULL DEFAULT 0 CHECK (disk_quota_mb >= 0),
    -- Empty means "whatever the panel default is".
    default_php   TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL
);

-- ON DELETE SET NULL rather than RESTRICT: removing a package should not be
-- blocked by the accounts on it, and an account with no package is simply
-- unlimited until one is assigned.
ALTER TABLE users ADD COLUMN plan_id INTEGER REFERENCES plans(id) ON DELETE SET NULL;

-- +goose Down
DROP TABLE plans;
