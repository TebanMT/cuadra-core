-- Migration 004 — owner_alert_configs (sidecar mirror of postgres/005).
--
-- Per-gym owner-alert toggle. Absence of a row means "use default" (enabled).
-- See notifications/domain/alertconfig for the canonical key list.
-- Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS owner_alert_configs (
    gym_id      TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    alert_key   TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    version     INTEGER NOT NULL DEFAULT 1,
    updated_at  INTEGER NOT NULL,
    deleted_at  INTEGER,
    synced_at   INTEGER,

    PRIMARY KEY (gym_id, alert_key)
);
CREATE INDEX IF NOT EXISTS idx_owner_alert_configs_sync ON owner_alert_configs(gym_id, updated_at);

INSERT INTO _migrations (version, name, applied_at)
SELECT 4, '004_owner_alert_configs', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 4);

COMMIT;
