-- Migration 005 — owner_alert_configs (UC-040 DA-40.1).
--
-- Per-gym toggle for the owner-alert keys the FE renders in the settings
-- page. Defaults live in code (notifications/domain/alertconfig.Defaults);
-- a row in this table exists ONLY when the owner has changed the toggle
-- away from its default. Absence == default == enabled. Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS owner_alert_configs (
    gym_id      UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    alert_key   TEXT NOT NULL,
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    version     INTEGER NOT NULL DEFAULT 1,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ,

    PRIMARY KEY (gym_id, alert_key)
);

CREATE INDEX IF NOT EXISTS idx_owner_alert_configs_sync ON owner_alert_configs(gym_id, updated_at);

INSERT INTO _migrations (version, name) VALUES (5, '005_owner_alert_configs')
ON CONFLICT (version) DO NOTHING;

COMMIT;
