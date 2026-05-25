-- Migration 028 — Promociones (plan Standard).
--
-- Permite al dueño crear descuentos, cupones, 2x1 y días gratis para
-- aplicar al cobrar. Dos tablas: `promotions` (catálogo del gym) y
-- `applied_promotions` (log inmutable de cada aplicación, con snapshot
-- del estado de la promo al momento del cobro). MAX 1 promo por cobro
-- — enforce en use case, no en schema (la combinatoria de stacking
-- queda como decisión de producto futura).
--
-- Mecánicas (`kind`):
--   percent              -> value es porcentaje 0-100
--   fixed_amount         -> value en pesos (numeric)
--   free_enrollment      -> omite enrollment_fee del plan; value NULL
--   extra_days           -> +N días al expiry; value en días
--   companion_memberships -> M membresías $0 a OTROS socios; value NULL,
--                            companion_count NOT NULL
--
-- N×M: buy_n queda en schema para futuro N>1 sin migración nueva
-- (default 1 hoy, no expuesto en form).
--
-- Cupones: code es opcional y único por gym cuando no-null
-- (case-insensitive — operador escribe "verano2026" o "VERANO2026"
-- indistinto). Usamos LOWER(code) en el partial unique index.

BEGIN;

CREATE TABLE IF NOT EXISTS promotions (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    name            TEXT NOT NULL,
    description     TEXT,
    kind            TEXT NOT NULL,
    value           NUMERIC(12,2),
    buy_n           INTEGER NOT NULL DEFAULT 1,
    companion_count INTEGER,
    applies_to      TEXT NOT NULL,
    code            TEXT,
    valid_from      DATE,
    valid_until     DATE,
    max_uses_total      INTEGER,
    max_uses_per_member INTEGER,
    active          BOOLEAN NOT NULL DEFAULT TRUE,

    CONSTRAINT chk_promotions_name_len CHECK (LENGTH(name) BETWEEN 3 AND 100),
    CONSTRAINT chk_promotions_kind CHECK (kind IN (
        'percent','fixed_amount','free_enrollment','extra_days','companion_memberships'
    )),
    CONSTRAINT chk_promotions_applies_to CHECK (applies_to IN ('membership','sale','any')),
    CONSTRAINT chk_promotions_buy_n CHECK (buy_n >= 1),
    CONSTRAINT chk_promotions_max_uses_total
        CHECK (max_uses_total IS NULL OR max_uses_total > 0),
    CONSTRAINT chk_promotions_max_uses_per_member
        CHECK (max_uses_per_member IS NULL OR max_uses_per_member > 0),
    CONSTRAINT chk_promotions_value_by_kind CHECK (
        (kind = 'percent'               AND value IS NOT NULL AND value BETWEEN 0 AND 100)
     OR (kind = 'fixed_amount'          AND value IS NOT NULL AND value > 0)
     OR (kind = 'extra_days'            AND value IS NOT NULL AND value >= 1)
     OR (kind = 'free_enrollment'       AND value IS NULL)
     OR (kind = 'companion_memberships' AND value IS NULL AND companion_count IS NOT NULL AND companion_count >= 1)
    ),
    CONSTRAINT chk_promotions_dates CHECK (
        valid_from IS NULL OR valid_until IS NULL OR valid_until >= valid_from
    )
);

CREATE INDEX IF NOT EXISTS idx_promotions_gym
    ON promotions(gym_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_promotions_sync
    ON promotions(gym_id, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_promotions_gym_code
    ON promotions(gym_id, LOWER(code))
    WHERE code IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS applied_promotions (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    promotion_id            UUID NOT NULL REFERENCES promotions(id) ON DELETE RESTRICT,
    payment_id              UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    member_id               UUID REFERENCES members(id) ON DELETE RESTRICT,
    applied_by_user_id      UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    promotion_name_snapshot TEXT NOT NULL,
    kind_snapshot           TEXT NOT NULL,
    value_snapshot          NUMERIC(12,2),
    discount_amount         NUMERIC(12,2) NOT NULL DEFAULT 0,
    extra_days_applied      INTEGER NOT NULL DEFAULT 0,
    notes                   TEXT,

    CONSTRAINT chk_applied_promotions_discount   CHECK (discount_amount >= 0),
    CONSTRAINT chk_applied_promotions_extra_days CHECK (extra_days_applied >= 0)
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

INSERT INTO _migrations (version, name) VALUES (28, '028_promotions')
ON CONFLICT (version) DO NOTHING;

COMMIT;
