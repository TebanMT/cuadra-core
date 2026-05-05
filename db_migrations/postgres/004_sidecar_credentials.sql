-- Migration 004 — sidecar_credentials (cloud-only, ADR-008 §3.2)
--
-- Long-lived opaque tokens that authenticate sidecar↔cloud sync traffic.
-- Replaces the operator JWT for /sync/* (the JWT keeps its role for
-- dashboard↔cloud and desktop UI↔sidecar). One active token per
-- (gym_id, client_id) pair — re-pairing the same machine revokes the prior
-- one and emits a new one. last_seen_at drives the auto-revoke cron
-- (ADR-008 §3.5).

BEGIN;

CREATE TABLE IF NOT EXISTS sidecar_credentials (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE CASCADE,
    client_id       UUID NOT NULL,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    key_hash        BYTEA NOT NULL,
    device_label    TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at      TIMESTAMPTZ
);

-- One active credential per (gym_id, client_id). Re-pairing the same
-- machine must first revoke the existing row (UPDATE revoked_at) before
-- INSERT, otherwise this constraint fires.
CREATE UNIQUE INDEX IF NOT EXISTS sidecar_credentials_active_per_client
    ON sidecar_credentials (gym_id, client_id) WHERE revoked_at IS NULL;

-- Lookup index for the middleware: SELECT WHERE key_hash = SHA256(token)
-- AND revoked_at IS NULL.
CREATE INDEX IF NOT EXISTS sidecar_credentials_key_hash
    ON sidecar_credentials (key_hash) WHERE revoked_at IS NULL;

-- Cron sweeps WHERE revoked_at IS NULL AND last_seen_at < now() - 30d, so
-- a covering index on (revoked_at, last_seen_at) keeps it cheap.
CREATE INDEX IF NOT EXISTS sidecar_credentials_idle
    ON sidecar_credentials (last_seen_at) WHERE revoked_at IS NULL;

INSERT INTO _migrations (version, name) VALUES (4, '004_sidecar_credentials')
ON CONFLICT (version) DO NOTHING;

COMMIT;
