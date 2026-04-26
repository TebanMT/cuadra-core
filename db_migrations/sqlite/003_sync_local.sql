-- Migration 003 — sidecar local sync support tables (ADR-001 Sesión 8).
--
-- Adds:
--   * conflict_log_local — mirror of cloud `conflict_log`, populated by the
--     sync agent whenever the server returns conflict_server_wins or
--     conflict_client_wins so support can debug from the laptop.
--   * client_id stored in sync_state for stable identity across restarts
--     (the agent generates one on first start if absent).
--
-- Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS conflict_log_local (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    gym_id          TEXT NOT NULL,
    entity_type     TEXT NOT NULL,
    entity_id       TEXT NOT NULL,
    client_version  INTEGER NOT NULL,
    server_version  INTEGER NOT NULL,
    client_payload  TEXT NOT NULL,
    server_payload  TEXT NOT NULL,
    resolution      TEXT NOT NULL CHECK (resolution IN ('server_wins','client_wins')),
    detected_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conflict_log_local_gym ON conflict_log_local(gym_id, detected_at DESC);

INSERT INTO _migrations (version, name, applied_at)
SELECT 3, '003_sync_local', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 3);

COMMIT;
