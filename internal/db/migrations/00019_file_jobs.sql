-- +goose Up

-- Long-running file operations.
--
-- Packing a few gigabytes or unpacking a large archive takes minutes. Run on
-- the request that asked for it, the work stops the moment the browser goes
-- away -- a closed tab, a phone changing network, a laptop lid -- and leaves
-- a half-written archive with nothing to say what happened. A row here is
-- written before the work starts and closed out after, so the answer to
-- "what happened to that" survives the connection that asked.
CREATE TABLE file_jobs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- archive | extract | upload
    kind        TEXT    NOT NULL,
    -- running | done | failed
    status      TEXT    NOT NULL DEFAULT 'running',
    -- What it is working on, shown while it runs and kept after.
    target      TEXT    NOT NULL DEFAULT '',
    detail      TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    created_at  TEXT    NOT NULL,
    finished_at TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX file_jobs_owner ON file_jobs (owner_id, created_at);

-- +goose Down
DROP TABLE file_jobs;
