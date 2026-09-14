-- +goose Up

-- Where backups are copied to, besides this server.
--
-- A backup that only exists on the machine it was taken from is not a backup
-- of that machine; it is a copy of a directory. These rows hold everything
-- needed to show a destination in a list and nothing that could be used to
-- reach it: the password, private key or secret key lives in a root-only
-- file beside the agent, the same way the panel never holds a customer's
-- database password. A copy of this database is not a copy of anybody's
-- storage credentials.
CREATE TABLE backup_destinations (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    -- Whose backups go here. 0 means the server's own, set by an
    -- administrator and used for every account that has none of its own.
    --
    -- Deliberately not a foreign key: 0 is a real value here and there is no
    -- user with that id, so a reference would refuse every server-wide
    -- destination. Rows for a deleted account are cleared in the trigger
    -- below instead.
    owner_id INTEGER NOT NULL DEFAULT 0,
    name     TEXT    NOT NULL,
    -- sftp | s3
    kind     TEXT    NOT NULL,
    -- Host, port, user, path, bucket, region, endpoint, access key: enough
    -- to identify the destination, never enough to use it.
    summary  TEXT    NOT NULL DEFAULT '',
    enabled  INTEGER NOT NULL DEFAULT 1,
    last_ok_at  TEXT NOT NULL DEFAULT '',
    last_error  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);
CREATE INDEX idx_destinations_owner ON backup_destinations(owner_id);

-- What the foreign key would have done, without refusing owner_id 0.
-- +goose StatementBegin
CREATE TRIGGER destinations_follow_user_delete
AFTER DELETE ON users
BEGIN
    DELETE FROM backup_destinations WHERE owner_id = OLD.id AND owner_id != 0;
END;
-- +goose StatementEnd

-- Where a given backup ended up, so a restore knows what is still reachable.
CREATE TABLE backup_copies (
    backup_id      INTEGER NOT NULL REFERENCES backups(id) ON DELETE CASCADE,
    destination_id INTEGER NOT NULL REFERENCES backup_destinations(id) ON DELETE CASCADE,
    remote_path    TEXT    NOT NULL DEFAULT '',
    bytes          INTEGER NOT NULL DEFAULT 0,
    -- uploaded | failed
    status     TEXT NOT NULL,
    error      TEXT NOT NULL DEFAULT '',
    at         TEXT NOT NULL,
    PRIMARY KEY (backup_id, destination_id)
);

-- +goose Down
DROP TABLE backup_copies;
DROP TRIGGER destinations_follow_user_delete;
DROP INDEX idx_destinations_owner;
DROP TABLE backup_destinations;
