-- Migration 016 — operators PIN-first (sidecar mirror of postgres/019).
--
-- SQLite no soporta ALTER COLUMN ... DROP NOT NULL, así que reconstruimos
-- la tabla users con password_hash nullable. Otras tablas (members,
-- payments, checkins, audit_log, etc.) tienen FKs hacia users(id); el
-- rebuild se hace con foreign_keys=OFF para evitar que el DROP TABLE
-- limpie filas dependientes. Reactivamos FK + foreign_key_check al
-- final para verificar que no quedaron referencias rotas.

PRAGMA foreign_keys = OFF;

BEGIN;

CREATE TABLE users_new (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    email                TEXT NOT NULL,
    password_hash        TEXT,
    full_name            TEXT NOT NULL,
    phone                TEXT,
    role                 TEXT NOT NULL CHECK (role IN ('owner','operator')),
    active               INTEGER NOT NULL DEFAULT 1,
    must_change_password INTEGER NOT NULL DEFAULT 0,
    last_login_at        INTEGER,
    created_by           TEXT REFERENCES users(id) ON DELETE RESTRICT,

    pin_hash             TEXT,
    pin_assigned_at      INTEGER
);

INSERT INTO users_new
    (id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
     email, password_hash, full_name, phone, role, active,
     must_change_password, last_login_at, created_by,
     pin_hash, pin_assigned_at)
SELECT
    id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
    email, password_hash, full_name, phone, role, active,
    must_change_password, last_login_at, created_by,
    pin_hash, pin_assigned_at
FROM users;

DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

-- Email opcional para operadores: el índice único ignora "" (vacío)
-- para que varios operadores sin correo puedan coexistir. Mantenemos
-- la columna NOT NULL (string vacío = "sin correo") para reducir
-- ondas de cambios en mappers/projectors.
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_email
    ON users(email COLLATE NOCASE) WHERE email <> '' AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_users_gym
    ON users(gym_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_users_sync
    ON users(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_users_sync_pending
    ON users(synced_at) WHERE synced_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_gym_owner
    ON users(gym_id) WHERE role = 'owner' AND active = 1 AND deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_gym_phone
    ON users(gym_id, phone) WHERE phone IS NOT NULL AND deleted_at IS NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 16, '016_operators_pin_first', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 16);

COMMIT;

PRAGMA foreign_keys = ON;
