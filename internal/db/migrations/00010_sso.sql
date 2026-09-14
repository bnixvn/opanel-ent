-- +goose Up

-- One-time tickets for signing in to phpMyAdmin.
--
-- The panel never stores a customer's database password -- it is shown once
-- and only its hash exists in MariaDB -- so single sign-on cannot re-use it.
-- Each sign-in mints a throwaway MariaDB account instead, granted on exactly
-- the databases the customer owns, and this table is what the shim redeems
-- the ticket against.
CREATE TABLE sso_tickets (
    -- SHA-256 of the value in the URL, so a stolen database gives nobody a
    -- usable ticket.
    token_hash TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- The throwaway MariaDB account and its password. The password is here
    -- because the shim has to hand it to phpMyAdmin, and the account it
    -- belongs to is dropped minutes later.
    db_user     TEXT NOT NULL,
    db_password TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    -- Set the moment it is redeemed. A ticket works exactly once.
    used_at     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_sso_expires ON sso_tickets(expires_at);

-- +goose Down
DROP INDEX idx_sso_expires;
DROP TABLE sso_tickets;
