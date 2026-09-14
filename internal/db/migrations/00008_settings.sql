-- +goose Up

-- The settings table itself already exists (migration 00001). This migration
-- adds the two tables that go with it and leaves that one alone.

-- Hostnames the panel will answer on, one per row.
--
-- Serving the panel under any name that happens to resolve to this server
-- would let anybody with a spare domain put a login form for this panel on
-- their own address. A hostname has to be added here first.
CREATE TABLE panel_hostnames (
    hostname   TEXT PRIMARY KEY,
    cert_file  TEXT NOT NULL DEFAULT '',
    key_file   TEXT NOT NULL DEFAULT '',
    is_primary INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT ''
);

-- Where to send an alert, per account. Admins and resellers configure their
-- own; nothing is sent for an account with no row.
CREATE TABLE notification_targets (
    user_id          INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    telegram_token   TEXT NOT NULL DEFAULT '',
    telegram_chat_id TEXT NOT NULL DEFAULT '',
    -- Space-separated event names, empty meaning every event.
    events     TEXT NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE notification_targets;
DROP TABLE panel_hostnames;
