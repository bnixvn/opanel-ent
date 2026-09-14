-- +goose Up

-- Passkeys, one row per registered authenticator.
--
-- A passkey is a public key: the private half never leaves the phone or
-- security key it was created on, so this table holds nothing that could be
-- replayed if it were stolen. That is the point of the feature -- unlike a
-- password hash, there is no secret here to protect.
CREATE TABLE passkeys (
    -- The credential id the authenticator chose, base64url. Unique across
    -- accounts: an authenticator must not be registerable twice.
    id         TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- What the customer calls it: "my phone", "the yubikey in the drawer".
    label      TEXT    NOT NULL DEFAULT '',
    -- The COSE public key, base64.
    public_key TEXT    NOT NULL,
    -- The authenticator's own identifier, used to recognise a device that is
    -- already registered.
    aaguid     TEXT    NOT NULL DEFAULT '',
    -- The signature counter. An authenticator that reports a counter going
    -- backwards has been cloned, which is the one thing this protocol can
    -- detect on its own.
    sign_count INTEGER NOT NULL DEFAULT 0,
    -- Whether the key lives on the authenticator itself, which is what makes
    -- a username-less sign-in possible.
    resident   INTEGER NOT NULL DEFAULT 0,
    -- The transports the browser said it supports, space separated, so the
    -- next prompt can suggest the right one.
    transports TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL,
    last_used_at TEXT  NOT NULL DEFAULT ''
);
CREATE INDEX idx_passkeys_user ON passkeys(user_id);

-- In-flight registration and sign-in challenges.
--
-- A challenge is single use and short lived; kept in the database rather than
-- in memory so a panel restart between the two halves of a ceremony fails
-- cleanly instead of with an error nobody can explain.
CREATE TABLE passkey_challenges (
    id         TEXT    PRIMARY KEY,
    user_id    INTEGER NOT NULL DEFAULT 0,
    -- "register" or "login".
    purpose    TEXT    NOT NULL,
    -- The serialised session data the WebAuthn library needs to finish.
    session    TEXT    NOT NULL,
    expires_at TEXT    NOT NULL
);

-- +goose Down
DROP TABLE passkey_challenges;
DROP INDEX idx_passkeys_user;
DROP TABLE passkeys;
