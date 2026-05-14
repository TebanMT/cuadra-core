-- Migration 007 — expandir maintenance_frequency con bimensual, trimestral
-- y semestral. Espeja postgres/010. SQLite no permite ALTER de CHECK
-- constraints in-place, así que recreamos la tabla.

BEGIN;

CREATE TABLE membership_types_new (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    name                    TEXT NOT NULL,
    price                   INTEGER NOT NULL CHECK (price > 0),  -- cents
    duration_days           INTEGER NOT NULL CHECK (duration_days >= 1),
    enrollment_fee          INTEGER NOT NULL DEFAULT 0,
    maintenance_fee         INTEGER NOT NULL DEFAULT 0,
    maintenance_frequency   TEXT,
    active                  INTEGER NOT NULL DEFAULT 1,

    CHECK (
        (maintenance_fee = 0 AND maintenance_frequency IS NULL) OR
        (maintenance_fee > 0 AND maintenance_frequency IN
            ('monthly','bimonthly','quarterly','semiannual','annual'))
    )
);

INSERT INTO membership_types_new
    (id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
     name, price, duration_days, enrollment_fee, maintenance_fee,
     maintenance_frequency, active)
SELECT
    id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
    name, price, duration_days, enrollment_fee, maintenance_fee,
    maintenance_frequency, active
FROM membership_types;

DROP TABLE membership_types;
ALTER TABLE membership_types_new RENAME TO membership_types;

-- Recreamos los índices que vivían en la tabla original.
CREATE UNIQUE INDEX IF NOT EXISTS uq_membership_types_gym_name
    ON membership_types(gym_id, name COLLATE NOCASE) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_membership_types_sync
    ON membership_types(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_membership_types_sync_pending
    ON membership_types(synced_at) WHERE synced_at IS NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 7, '007_membership_freq_expand', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 7);

COMMIT;
