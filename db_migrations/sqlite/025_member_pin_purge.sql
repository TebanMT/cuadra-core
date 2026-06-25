-- Migration 025 — ADR-010: purga total del "PIN de socio". Espeja postgres/030.
--
--   1. Borra las columnas deprecadas members.pin_hash / pin_plain /
--      pin_assigned_at (SQLite 3.35+ soporta DROP COLUMN).
--   2. Renombra el método de check-in "pin" → "number". En SQLite el CHECK de
--      una columna no se puede alterar en sitio → rebuild de la tabla checkins
--      (no tiene FKs entrantes, así que es seguro).
-- El PIN de LOGIN del operador (users.pin_hash) es otro concepto y NO se toca.

BEGIN;

-- 1) Borrar columnas PIN deprecadas del socio.
ALTER TABLE members DROP COLUMN pin_hash;
ALTER TABLE members DROP COLUMN pin_plain;
ALTER TABLE members DROP COLUMN pin_assigned_at;

-- 2) checkins.method "pin" → "number" vía rebuild (cambia el CHECK).
CREATE TABLE checkins_new (
    id          TEXT PRIMARY KEY,
    gym_id      TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    deleted_at  INTEGER,
    synced_at   INTEGER,

    member_id           TEXT NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    checkin_at          INTEGER NOT NULL,
    method              TEXT NOT NULL CHECK (method IN ('fingerprint','manual','number')),
    result              TEXT NOT NULL CHECK (result IN ('allowed_active','allowed_expiring_soon','allowed_override','denied_expired','denied_inactive','denied_no_membership','denied_unpaid_enrollment')),
    operator_id         TEXT REFERENCES users(id) ON DELETE RESTRICT,
    manual_override     INTEGER NOT NULL DEFAULT 0,
    override_reason     TEXT
);

INSERT INTO checkins_new (
    id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
    member_id, checkin_at, method, result, operator_id, manual_override, override_reason
)
SELECT
    id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
    member_id, checkin_at,
    CASE method WHEN 'pin' THEN 'number' ELSE method END,
    result, operator_id, manual_override, override_reason
FROM checkins;

DROP TABLE checkins;
ALTER TABLE checkins_new RENAME TO checkins;

CREATE INDEX IF NOT EXISTS idx_checkins_member_date ON checkins(member_id, checkin_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_checkins_gym_date ON checkins(gym_id, checkin_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_checkins_sync ON checkins(gym_id, updated_at);

INSERT INTO _migrations (version, name, applied_at)
SELECT 25, '025_member_pin_purge', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 25);

COMMIT;
