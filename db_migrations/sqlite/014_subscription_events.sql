-- Migration 014 — clonar subscription_events en SQLite local del sidecar.
-- Espeja postgres/007_subscription_events.sql + postgres/017_subscription_events_sync.sql.
--
-- Append-only desde la perspectiva del sidecar: nunca escribimos rows acá
-- (los webhooks viven cloud-only). El projector genérico hace pull desde
-- cloud → SQLite vía sync_entities, escribiendo via INSERT ... ON CONFLICT.
-- El use case GetSubscription lee de aquí para llenar el historial de la
-- página Suscripción.

BEGIN;

CREATE TABLE IF NOT EXISTS subscription_events (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch('now') * 1000),
    updated_at      INTEGER NOT NULL DEFAULT (unixepoch('now') * 1000),
    deleted_at      INTEGER,
    synced_at       INTEGER,

    provider        TEXT NOT NULL CHECK (provider IN ('stripe','mercadopago','manual')),
    type            TEXT NOT NULL CHECK (type IN ('activated','renewed','past_due','cancelled','trial_extended')),
    external_id     TEXT NOT NULL,
    plan            TEXT NOT NULL,
    amount          REAL,
    currency        TEXT,
    period_ends_at  INTEGER,
    raw_payload     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(raw_payload)),
    occurred_at     INTEGER NOT NULL,
    recorded_at     INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_sub_event_external_local
    ON subscription_events(provider, external_id);

CREATE INDEX IF NOT EXISTS ix_sub_event_gym_occurred_local
    ON subscription_events(gym_id, occurred_at DESC);

CREATE INDEX IF NOT EXISTS ix_sub_event_sync_local
    ON subscription_events(updated_at);

INSERT INTO _migrations (version, name, applied_at)
SELECT 14, '014_subscription_events', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 14);

COMMIT;
