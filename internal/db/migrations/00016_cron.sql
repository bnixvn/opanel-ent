-- +goose Up

-- Scheduled commands, one row per job.
--
-- The panel owns each account's whole crontab and renders it from these
-- rows, the same way it owns the webserver's configuration. Anything already
-- in a crontab when the panel first looks is imported into rows, so taking
-- ownership loses nothing.
CREATE TABLE cron_jobs (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- The five-field form, normalised. @reboot and friends are refused.
    schedule TEXT    NOT NULL,
    command  TEXT    NOT NULL,
    -- What the customer calls it, rendered as a comment above the line.
    comment  TEXT    NOT NULL DEFAULT '',
    -- A disabled job stays as a commented line rather than disappearing, so
    -- the file explains itself to anyone reading it over SSH.
    enabled  INTEGER NOT NULL DEFAULT 1,
    created_at TEXT  NOT NULL,
    updated_at TEXT  NOT NULL
);
CREATE INDEX idx_cron_user ON cron_jobs(user_id);

-- Whether the panel has already taken over an account's crontab, so the
-- import from an existing file happens exactly once.
CREATE TABLE cron_imported (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    at      TEXT NOT NULL
);

-- +goose Down
DROP TABLE cron_imported;
DROP INDEX idx_cron_user;
DROP TABLE cron_jobs;
