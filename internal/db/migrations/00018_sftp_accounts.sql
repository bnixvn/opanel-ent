-- +goose Up

-- Extra SFTP credentials for one hosting account.
--
-- Each row is a Linux account of its own that shares the owner's home and
-- primary group, so it can be handed to a developer and withdrawn again
-- without touching the credential the owner themselves uses.
CREATE TABLE sftp_accounts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    username   TEXT    NOT NULL UNIQUE,
    note       TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
);

CREATE INDEX sftp_accounts_owner ON sftp_accounts (owner_id);

-- +goose Down
DROP TABLE sftp_accounts;
