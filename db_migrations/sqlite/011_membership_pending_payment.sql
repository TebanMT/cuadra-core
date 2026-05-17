-- Migration 011 — soporte para "membresía inscrita sin pago todavía".
-- Espeja postgres/014.
--
-- SQLite no soporta DROP CONSTRAINT en CHECKs, así que recreamos las
-- tablas memberships y checkins manteniendo todos los índices.
--
-- Patrón de rebuild: create-new → copy → drop-old → rename-new (igual
-- que las migraciones 007 y 016). NO rename-first: el `ALTER TABLE ...
-- RENAME TO` del SQLite moderno reescribe las FK refs de las tablas
-- hijas (p.ej. membership_adjustments.membership_id), así que renombrar
-- la tabla viva a un nombre temporal dejaba esas FKs apuntando a la
-- tabla temporal — y tras el DROP quedaban colgantes ("no such table:
-- _memberships_old" al primer write). Creando la tabla _new y
-- renombrándola al final, el único RENAME es new→canónico y no hay
-- refs a `_new` que reescribir.
--
-- foreign_keys=OFF durante el rebuild: el DROP TABLE de una tabla
-- referenciada haría un implicit DELETE que viola los FK RESTRICT de
-- las hijas. Se reactiva al final.
--
-- Cambios:
--   * status CHECK agrega 'pending_payment'.
--   * expiry_date pasa a NULL-able (membresías pending no tienen).
--   * dates CHECK permite expiry_date IS NULL.
--   * uq_memberships_member_active incluye 'pending_payment' en la
--     condición parcial (un socio sólo puede estar inscrito en un
--     plan a la vez, pagado o no).
--   * checkins.result añade 'denied_unpaid_enrollment'.

PRAGMA foreign_keys = OFF;

BEGIN;

-- ── memberships rebuild ────────────────────────────────────────────────
CREATE TABLE memberships_new (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    member_id               TEXT NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    membership_type_id      TEXT NOT NULL REFERENCES membership_types(id) ON DELETE RESTRICT,

    type_name_snapshot      TEXT NOT NULL,
    price_snapshot          INTEGER NOT NULL,
    duration_days_snapshot  INTEGER NOT NULL,

    start_date      TEXT NOT NULL,
    expiry_date     TEXT,
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','expired','replaced','cancelled','pending_payment')),
    replaced_by     TEXT REFERENCES memberships_new(id),

    CHECK (expiry_date IS NULL OR expiry_date >= start_date)
);

INSERT INTO memberships_new
SELECT id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
       member_id, membership_type_id, type_name_snapshot, price_snapshot,
       duration_days_snapshot, start_date, expiry_date, status, replaced_by
FROM memberships;

DROP TABLE memberships;
ALTER TABLE memberships_new RENAME TO memberships;

CREATE INDEX IF NOT EXISTS idx_memberships_member ON memberships(member_id, status) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memberships_gym_expiry ON memberships(gym_id, expiry_date) WHERE status = 'active' AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memberships_sync ON memberships(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_memberships_sync_pending ON memberships(synced_at) WHERE synced_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_memberships_member_active ON memberships(member_id) WHERE status IN ('active','pending_payment') AND deleted_at IS NULL;

-- ── checkins rebuild ───────────────────────────────────────────────────
CREATE TABLE checkins_new (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,
    member_id           TEXT NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    checkin_at          INTEGER NOT NULL,
    method              TEXT NOT NULL CHECK (method IN ('fingerprint','manual','pin')),
    result              TEXT NOT NULL CHECK (result IN ('allowed_active','allowed_expiring_soon','allowed_override','denied_expired','denied_inactive','denied_no_membership','denied_unpaid_enrollment')),
    operator_id         TEXT REFERENCES users(id) ON DELETE RESTRICT,
    manual_override     INTEGER NOT NULL DEFAULT 0,
    override_reason     TEXT
);

INSERT INTO checkins_new SELECT * FROM checkins;

DROP TABLE checkins;
ALTER TABLE checkins_new RENAME TO checkins;

CREATE INDEX IF NOT EXISTS idx_checkins_member_date ON checkins(member_id, checkin_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_checkins_gym_date ON checkins(gym_id, checkin_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_checkins_sync ON checkins(gym_id, updated_at);

INSERT INTO _migrations (version, name, applied_at)
SELECT 11, '011_membership_pending_payment', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 11);

COMMIT;

PRAGMA foreign_keys = ON;
