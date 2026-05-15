-- Migration 012 — gastos generales del gimnasio.
-- Espeja postgres/015_expenses.sql.
--
-- Convenciones SQLite (vs Postgres):
--   * amount en cents (INTEGER); el mapper sidecar convierte al edge.
--   * expense_date como TEXT YYYY-MM-DD (parseo lexicográfico = orden).
--   * created_at / updated_at / deleted_at / synced_at: epoch ms UTC.

BEGIN;

CREATE TABLE IF NOT EXISTS expenses (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    expense_date    TEXT NOT NULL,
    amount          INTEGER NOT NULL CHECK (amount > 0),
    category        TEXT NOT NULL CHECK (category IN (
        'renta','servicios','mantenimiento','sueldos','marketing','mercaderia_externa','otros'
    )),
    description     TEXT,
    payment_method  TEXT NOT NULL CHECK (payment_method IN ('cash','transfer','card')),
    created_by      TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    CHECK (description IS NULL OR length(description) <= 200)
);

CREATE INDEX IF NOT EXISTS idx_expenses_gym_date ON expenses(gym_id, expense_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_expenses_gym_category ON expenses(gym_id, category) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_expenses_sync ON expenses(gym_id, updated_at);

INSERT INTO _migrations (version, name, applied_at)
SELECT 12, '012_expenses', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 12);

COMMIT;
