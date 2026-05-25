-- Migration 023 — Promociones (par de postgres/028).
--
-- Mismas tablas que el cloud, con tipos SQLite (TEXT uuid, INTEGER bool,
-- INTEGER cents para value/discount_amount, INTEGER ms para fechas). Para
-- valid_from / valid_until usamos TEXT 'YYYY-MM-DD' en lugar de INTEGER ms
-- porque son fechas calendario (no timestamps) y se comparan contra "hoy"
-- en el use case sin necesidad de math de epoch.
--
-- Reusamos COLLATE NOCASE para la unicidad case-insensitive del code,
-- equivalente al LOWER() del cloud.

BEGIN;

CREATE TABLE IF NOT EXISTS promotions (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    name            TEXT NOT NULL CHECK (LENGTH(name) BETWEEN 3 AND 100),
    description     TEXT,
    kind            TEXT NOT NULL CHECK (kind IN (
        'percent','fixed_amount','free_enrollment','extra_days','companion_memberships'
    )),
    value           INTEGER,   -- percent: 0-100; fixed_amount: cents; extra_days: días
    buy_n           INTEGER NOT NULL DEFAULT 1 CHECK (buy_n >= 1),
    companion_count INTEGER,
    applies_to      TEXT NOT NULL CHECK (applies_to IN ('membership','sale','any')),
    code            TEXT,
    valid_from      TEXT,       -- 'YYYY-MM-DD'
    valid_until     TEXT,
    max_uses_total      INTEGER CHECK (max_uses_total IS NULL OR max_uses_total > 0),
    max_uses_per_member INTEGER CHECK (max_uses_per_member IS NULL OR max_uses_per_member > 0),
    active          INTEGER NOT NULL DEFAULT 1,

    CHECK (
        (kind = 'percent'               AND value IS NOT NULL AND value BETWEEN 0 AND 100)
     OR (kind = 'fixed_amount'          AND value IS NOT NULL AND value > 0)
     OR (kind = 'extra_days'            AND value IS NOT NULL AND value >= 1)
     OR (kind = 'free_enrollment'       AND value IS NULL)
     OR (kind = 'companion_memberships' AND value IS NULL AND companion_count IS NOT NULL AND companion_count >= 1)
    ),
    CHECK (
        valid_from IS NULL OR valid_until IS NULL OR valid_until >= valid_from
    )
);

CREATE INDEX IF NOT EXISTS idx_promotions_gym
    ON promotions(gym_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_promotions_sync
    ON promotions(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_promotions_sync_pending
    ON promotions(synced_at) WHERE synced_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_promotions_gym_code
    ON promotions(gym_id, code COLLATE NOCASE)
    WHERE code IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS applied_promotions (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    promotion_id            TEXT NOT NULL REFERENCES promotions(id) ON DELETE RESTRICT,
    payment_id              TEXT NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    member_id               TEXT REFERENCES members(id) ON DELETE RESTRICT,
    applied_by_user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    promotion_name_snapshot TEXT NOT NULL,
    kind_snapshot           TEXT NOT NULL,
    value_snapshot          INTEGER,
    discount_amount         INTEGER NOT NULL DEFAULT 0 CHECK (discount_amount >= 0),
    extra_days_applied      INTEGER NOT NULL DEFAULT 0 CHECK (extra_days_applied >= 0),
    notes                   TEXT
);

CREATE INDEX IF NOT EXISTS idx_applied_promotions_promotion
    ON applied_promotions(promotion_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_applied_promotions_payment
    ON applied_promotions(payment_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_applied_promotions_member
    ON applied_promotions(member_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_applied_promotions_gym_created
    ON applied_promotions(gym_id, created_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_applied_promotions_sync
    ON applied_promotions(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_applied_promotions_sync_pending
    ON applied_promotions(synced_at) WHERE synced_at IS NULL;

INSERT INTO _migrations (version, name, applied_at)
SELECT 23, '023_promotions', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 23);

COMMIT;
