-- +goose Up

-- MariaDB databases the panel created. The panel's own record of them; the
-- server itself is the authority on what exists, and a reconcile can compare
-- the two.
CREATE TABLE db_databases (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    -- Full name as it exists in MariaDB, including the owner prefix.
    name       TEXT    NOT NULL UNIQUE,
    owner_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TEXT    NOT NULL
);
CREATE INDEX idx_db_databases_owner ON db_databases(owner_id);

-- MariaDB accounts. Separate from databases, and linked by grants, because
-- that is the model people expect: one account may reach several databases,
-- and a database may be reachable by an application account and a
-- read-only reporting account.
CREATE TABLE db_users (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    username   TEXT    NOT NULL UNIQUE,
    owner_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TEXT    NOT NULL
);
CREATE INDEX idx_db_users_owner ON db_users(owner_id);

CREATE TABLE db_grants (
    database_id INTEGER NOT NULL REFERENCES db_databases(id) ON DELETE CASCADE,
    db_user_id  INTEGER NOT NULL REFERENCES db_users(id) ON DELETE CASCADE,
    created_at  TEXT    NOT NULL,
    PRIMARY KEY (database_id, db_user_id)
);

-- +goose Down
DROP TABLE db_grants;
DROP TABLE db_users;
DROP TABLE db_databases;
