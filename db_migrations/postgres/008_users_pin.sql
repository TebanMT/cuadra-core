-- Migration 008 — users.pin_hash + pin_assigned_at
--
-- The desktop's recepción flow refactor (auth-refactor v0.7) makes PIN the
-- primary credential for operator login at the reception laptop. Email +
-- password remain as a fallback (forgot-PIN, first-time-no-PIN). The PIN
-- itself is 4 digits; we store bcrypt(pin || gym-salt-not-applicable) — the
-- shared/auth helpers handle hashing.
--
-- MVP scope: only the owner uses PIN to log in (option C of the refactor
-- plan). The column is added to `users` rather than a separate table so
-- multi-operator can light up later by just exposing the assignment UI to
-- owners — no schema migration needed.
--
-- Both columns must be projector-synced so the desktop's local SQLite
-- mirror can answer login-pin offline (see shared/sync/tables.go).

BEGIN;

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS pin_hash         TEXT,
    ADD COLUMN IF NOT EXISTS pin_assigned_at  TIMESTAMPTZ;

INSERT INTO _migrations (version, name) VALUES (8, '008_users_pin')
ON CONFLICT (version) DO NOTHING;

COMMIT;
