-- +goose Up

-- Who a customer belongs to. NULL means the account belongs to the server
-- itself: administrators, and end users an administrator created directly.
--
-- One level only. A reseller cannot create another reseller, so this is a
-- forest of depth two and every query about "what may this caller see"
-- stays a single join instead of a recursive CTE.
ALTER TABLE users ADD COLUMN parent_id INTEGER REFERENCES users(id) ON DELETE SET NULL;
CREATE INDEX idx_users_parent ON users(parent_id);

-- What a reseller may hand out in total.
--
-- These are limits on what is *allocated*, not on what is used. Selling more
-- than the server holds is a business decision a host makes deliberately, so
-- it is refused by default and allowed per reseller with allow_oversell.
CREATE TABLE reseller_limits (
    user_id          INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_accounts     INTEGER NOT NULL DEFAULT 0,  -- 0 means no limit
    max_sites        INTEGER NOT NULL DEFAULT 0,
    max_databases    INTEGER NOT NULL DEFAULT 0,
    disk_quota_mb    INTEGER NOT NULL DEFAULT 0,
    bandwidth_gb     INTEGER NOT NULL DEFAULT 0,
    can_create_plans INTEGER NOT NULL DEFAULT 1,
    allow_oversell   INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT    NOT NULL DEFAULT '',
    updated_at       TEXT    NOT NULL DEFAULT ''
);

-- A package belongs to whoever created it. NULL is a system package, which
-- every reseller may assign; a reseller's own package is theirs alone.
ALTER TABLE plans ADD COLUMN owner_id INTEGER REFERENCES users(id) ON DELETE CASCADE;
CREATE INDEX idx_plans_owner ON plans(owner_id);

-- +goose Down
DROP INDEX idx_plans_owner;
ALTER TABLE plans DROP COLUMN owner_id;
DROP TABLE reseller_limits;
DROP INDEX idx_users_parent;
ALTER TABLE users DROP COLUMN parent_id;
