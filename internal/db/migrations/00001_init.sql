-- +goose Up

-- Panel users. A panel user maps 1:1 to a Linux user from Phase 2 onward;
-- linux_uid stays NULL until that account is provisioned.
CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    email         TEXT    NOT NULL DEFAULT '',
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL CHECK (role IN ('admin','reseller','end_user')),
    linux_uid     INTEGER,
    totp_secret   TEXT    NOT NULL DEFAULT '',
    totp_enabled  INTEGER NOT NULL DEFAULT 0 CHECK (totp_enabled IN (0,1)),
    suspended     INTEGER NOT NULL DEFAULT 0 CHECK (suspended IN (0,1)),
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL
);

-- One row per unused recovery code. Codes are single-use: the row is deleted
-- when redeemed, so a leaked database cannot show which codes were spent.
CREATE TABLE totp_recovery_codes (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash TEXT    NOT NULL,
    UNIQUE (user_id, code_hash)
);

-- Server-side sessions. id is the SHA-256 of the cookie value, never the
-- value itself, so database read access does not hand over live sessions.
CREATE TABLE sessions (
    id           TEXT    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TEXT    NOT NULL,
    expires_at   TEXT    NOT NULL,
    last_seen_at TEXT    NOT NULL,
    ip           TEXT    NOT NULL DEFAULT '',
    user_agent   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_sessions_user    ON sessions(user_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- API tokens for WHMCS and other machine callers. Same rule as sessions:
-- only the hash is stored. prefix is the public lookup handle.
CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    prefix       TEXT    NOT NULL UNIQUE,
    token_hash   TEXT    NOT NULL,
    scopes       TEXT    NOT NULL DEFAULT '',
    created_at   TEXT    NOT NULL,
    expires_at   TEXT,
    last_used_at TEXT,
    revoked_at   TEXT
);
CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);

-- Append-only audit trail. Every privileged agent action and every
-- authentication decision lands here.
CREATE TABLE audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    at         TEXT    NOT NULL,
    actor_type TEXT    NOT NULL,
    actor_id   INTEGER,
    actor_name TEXT    NOT NULL DEFAULT '',
    action     TEXT    NOT NULL,
    target     TEXT    NOT NULL DEFAULT '',
    ok         INTEGER NOT NULL CHECK (ok IN (0,1)),
    detail     TEXT    NOT NULL DEFAULT '',
    ip         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_audit_at     ON audit_log(at);
CREATE INDEX idx_audit_action ON audit_log(action);

-- Panel-wide key/value settings.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE settings;
DROP TABLE audit_log;
DROP TABLE api_tokens;
DROP TABLE sessions;
DROP TABLE totp_recovery_codes;
DROP TABLE users;
