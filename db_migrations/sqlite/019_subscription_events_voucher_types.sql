-- Migration 019 — espejar postgres/024: añadir voucher_emitted y
-- voucher_expired al CHECK de subscription_events.type.
--
-- SQLite no permite alterar un CHECK constraint in-place; hay que recrear
-- la tabla. La data existente se preserva. Los índices se recrean al final.
--
-- El sidecar recibe estos eventos vía sync (pull desde cloud), así que sin
-- esta migración la primera proyección de un voucher_emitted/expired
-- fallaría con CHECK constraint failed.

BEGIN;

CREATE TABLE subscription_events_new (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch('now') * 1000),
    updated_at      INTEGER NOT NULL DEFAULT (unixepoch('now') * 1000),
    deleted_at      INTEGER,
    synced_at       INTEGER,

    provider        TEXT NOT NULL CHECK (provider IN ('stripe','mercadopago','manual')),
    type            TEXT NOT NULL CHECK (type IN (
        'activated','renewed','past_due','cancelled','trial_extended',
        'voucher_emitted','voucher_expired'
    )),
    external_id     TEXT NOT NULL,
    plan            TEXT NOT NULL,
    amount          REAL,
    currency        TEXT,
    period_ends_at  INTEGER,
    raw_payload     TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(raw_payload)),
    occurred_at     INTEGER NOT NULL,
    recorded_at     INTEGER NOT NULL
);

INSERT INTO subscription_events_new
SELECT id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
       provider, type, external_id, plan, amount, currency, period_ends_at,
       raw_payload, occurred_at, recorded_at
FROM subscription_events;

DROP TABLE subscription_events;
ALTER TABLE subscription_events_new RENAME TO subscription_events;

CREATE UNIQUE INDEX IF NOT EXISTS ux_sub_event_external_local
    ON subscription_events(provider, external_id);
CREATE INDEX IF NOT EXISTS ix_sub_event_gym_occurred_local
    ON subscription_events(gym_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS ix_sub_event_sync_local
    ON subscription_events(updated_at);

INSERT INTO _migrations (version, name, applied_at)
SELECT 19, '019_subscription_events_voucher_types', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 19);

COMMIT;
