-- +goose Up

-- Who a session is really being driven by.
--
-- Zero for an ordinary session. When a member of staff signs in as a
-- customer, the new session belongs to the customer -- every permission
-- check then sees the customer, which is the whole point -- and this column
-- remembers who opened it, so the panel can say so on every page, refuse the
-- handful of things staff must not do while wearing somebody else's account,
-- and hand the operator back to their own session afterwards.
ALTER TABLE sessions ADD COLUMN impersonator_id INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN impersonator_id;
