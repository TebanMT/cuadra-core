-- Migration 021 — arregla la self-FK colgante de memberships.replaced_by.
--
-- La 011 construyó la tabla temporal como:
--
--     CREATE TABLE memberships_new (
--         ...
--         replaced_by TEXT REFERENCES memberships_new(id),
--         ...
--     );
--
-- y luego `ALTER TABLE memberships_new RENAME TO memberships`. SQLite docs
-- (https://www.sqlite.org/lang_altertable.html, "Other kinds of ALTER
-- TABLE"):
--
--   > The 'ALTER TABLE RENAME TO' command also modifies the SQL stored in
--   > sqlite_schema for all triggers, views, and foreign-key-constraints
--   > that reference the renamed table. The modification of the
--   > foreign-key-constraints only happens when the foreign_keys pragma
--   > is enabled, however.
--
-- La 011 corre con PRAGMA foreign_keys=OFF (necesario para que el DROP
-- TABLE no dispare cascades de RESTRICT), así que el rename NO reescribió
-- la self-FK. Quedó textualmente `REFERENCES memberships_new(id)` en
-- sqlite_schema, apuntando a una tabla que ya no existe. Cualquier
-- INSERT/UPDATE que dispare validación de FK explota con:
--
--   no such table: main.memberships_new
--
-- Esta migración rebuildea memberships UNA vez más, pero esta vez la
-- self-FK se declara con el nombre canónico final (`memberships(id)`,
-- NO `memberships_canon(id)`). Como la tabla temporal se llama
-- `memberships_canon`, SQLite busca el token `memberships_canon` durante
-- el rename y NO encuentra coincidencias dentro del body — la self-FK
-- queda intacta apuntando a `memberships`, que es donde tiene que
-- apuntar tras el rename.
--
-- Idempotente: el runner skipea archivos con version registrada en
-- _migrations, y este SQL es un rebuild completo — correrlo sobre una
-- DB ya arreglada produciría el mismo schema final. El INSERT al
-- _migrations se gatea con WHERE NOT EXISTS por las dudas.

PRAGMA foreign_keys = OFF;

BEGIN;

CREATE TABLE memberships_canon (
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
    -- self-FK apuntando al nombre canónico final. Ver header.
    replaced_by     TEXT REFERENCES memberships(id),

    CHECK (expiry_date IS NULL OR expiry_date >= start_date)
);

INSERT INTO memberships_canon
SELECT id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
       member_id, membership_type_id, type_name_snapshot, price_snapshot,
       duration_days_snapshot, start_date, expiry_date, status, replaced_by
FROM memberships;

DROP TABLE memberships;
ALTER TABLE memberships_canon RENAME TO memberships;

-- Misma lista de índices que 011 — idempotentes via IF NOT EXISTS aunque
-- el DROP TABLE de arriba ya los tiró con la tabla vieja.
CREATE INDEX IF NOT EXISTS idx_memberships_member ON memberships(member_id, status) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memberships_gym_expiry ON memberships(gym_id, expiry_date) WHERE status = 'active' AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memberships_sync ON memberships(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_memberships_sync_pending ON memberships(synced_at) WHERE synced_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_memberships_member_active ON memberships(member_id) WHERE status IN ('active','pending_payment') AND deleted_at IS NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 21, '021_fix_memberships_self_fk', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 21);

COMMIT;

PRAGMA foreign_keys = ON;
