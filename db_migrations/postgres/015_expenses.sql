-- Migration 015 — gastos generales del gimnasio.
-- Espeja sqlite/012_expenses.sql.
--
-- Captura egresos NO relacionados a mercancía: renta, servicios,
-- mantenimiento, sueldos, marketing, mercadería externa, otros.
-- Los egresos por mercancía siguen viviendo en stock_movements
-- (BC products) — ambas fuentes se agregan en el dashboard y los
-- reportes de período.

BEGIN;

CREATE TABLE IF NOT EXISTS expenses (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    expense_date    DATE NOT NULL,
    amount          NUMERIC(12,2) NOT NULL,
    category        TEXT NOT NULL,
    description     TEXT,
    payment_method  TEXT NOT NULL,
    created_by      UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    CONSTRAINT chk_expenses_amount CHECK (amount > 0),
    CONSTRAINT chk_expenses_category CHECK (category IN (
        'renta','servicios','mantenimiento','sueldos','marketing','mercaderia_externa','otros'
    )),
    CONSTRAINT chk_expenses_payment_method CHECK (payment_method IN ('cash','transfer','card')),
    CONSTRAINT chk_expenses_description_len CHECK (description IS NULL OR char_length(description) <= 200)
);

CREATE INDEX IF NOT EXISTS idx_expenses_gym_date ON expenses(gym_id, expense_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_expenses_gym_category ON expenses(gym_id, category) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_expenses_sync ON expenses(gym_id, updated_at);

INSERT INTO _migrations (version, name) VALUES (15, '015_expenses')
ON CONFLICT (version) DO NOTHING;

COMMIT;
