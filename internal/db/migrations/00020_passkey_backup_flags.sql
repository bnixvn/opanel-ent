-- +goose Up

-- The backup flags a WebAuthn credential was registered with.
--
-- go-webauthn refuses a sign-in when the BackupEligible flag on the assertion
-- differs from the one recorded at registration, which is a sound check and
-- one this table gave it no way to pass: the flags were never stored, so
-- every credential read back claimed to be ineligible. A phone passkey is
-- always eligible, so every one of them was rejected -- registration worked,
-- sign-in never did.
--
-- Nullable on purpose. NULL means "registered before this column existed",
-- which is not the same as false, and treating it as false is exactly the
-- bug. A row in that state adopts whatever the first assertion presents.
ALTER TABLE passkeys ADD COLUMN backup_eligible INTEGER;
ALTER TABLE passkeys ADD COLUMN backup_state INTEGER;

-- +goose Down
ALTER TABLE passkeys DROP COLUMN backup_state;
ALTER TABLE passkeys DROP COLUMN backup_eligible;
