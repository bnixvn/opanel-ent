-- +goose Up

-- Backups the panel knows about. The archive on disk is the real thing; this
-- table is the index over it, and it carries the state a long-running job
-- needs so a page refresh during a backup shows progress rather than nothing.
CREATE TABLE backups (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- File name under the owner's backup directory. Unique per owner rather
    -- than globally: two customers may both have monday.tar.gz.
    filename     TEXT    NOT NULL,
    -- manual | scheduled | uploaded. Kept so retention can prune the
    -- scheduled ones without touching a backup somebody took by hand before
    -- a risky change.
    kind         TEXT    NOT NULL DEFAULT 'manual',
    -- running | ready | failed. A row is written before the work starts, so
    -- a crash mid-backup leaves evidence instead of silence.
    status       TEXT    NOT NULL DEFAULT 'running',
    error        TEXT    NOT NULL DEFAULT '',
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    file_count   INTEGER NOT NULL DEFAULT 0,
    databases    TEXT    NOT NULL DEFAULT '',
    has_files    INTEGER NOT NULL DEFAULT 1,
    sha256       TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL,
    finished_at  TEXT    NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_backups_owner_file ON backups(owner_id, filename);
CREATE INDEX idx_backups_owner ON backups(owner_id, created_at);

-- One schedule per account. Not a cron expression: a hosting customer picks
-- "daily at 3am", and a full expression is a support burden that buys
-- nothing here.
CREATE TABLE backup_schedules (
    owner_id     INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    enabled      INTEGER NOT NULL DEFAULT 1,
    -- daily | weekly
    frequency    TEXT    NOT NULL DEFAULT 'daily',
    -- Hour of the day in the server's timezone, 0-23.
    hour         INTEGER NOT NULL DEFAULT 3,
    -- Day of the week for a weekly schedule, 0 = Sunday.
    weekday      INTEGER NOT NULL DEFAULT 0,
    -- How many scheduled archives to keep. Older ones are removed after a
    -- successful run, never before: a retention sweep that deletes first
    -- would leave an account with nothing if the new backup then fails.
    keep         INTEGER NOT NULL DEFAULT 7,
    include_files     INTEGER NOT NULL DEFAULT 1,
    include_databases INTEGER NOT NULL DEFAULT 1,
    last_run_at  TEXT    NOT NULL DEFAULT '',
    last_error   TEXT    NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE backup_schedules;
DROP TABLE backups;
