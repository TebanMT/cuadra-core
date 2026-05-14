-- Migration 005 — users.pin_hash + pin_assigned_at (sidecar mirror of pg/008).
--
-- See postgres/008_users_pin.sql for rationale. On the sidecar the columns
-- power offline PIN login: the proxy's /auth/login-pin reads pin_hash from
-- this local row and validates without a cloud round-trip, which is the
-- whole point of doing this at the sidecar layer.

BEGIN;

ALTER TABLE users ADD COLUMN pin_hash         TEXT;
ALTER TABLE users ADD COLUMN pin_assigned_at  INTEGER;

INSERT INTO _migrations (version, name, applied_at)
SELECT 5, '005_users_pin', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 5);

COMMIT;
