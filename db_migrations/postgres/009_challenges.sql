-- Migration 009 — challenges (retos)
--
-- "Retos" feature: a structured gym competition with composite scoring
-- (Índice de Recomposición). First edition runs in production at Gym Bros
-- with $20K MXN in prizes — bugs here destroy a real event, so the schema
-- is conservative and explicit.
--
-- Design points worth flagging:
--   - Tenancy + sync scoping on every table via gym_id (Tinta convention).
--   - Soft delete (deleted_at) everywhere. The projector + sidecar mirror
--     rely on it.
--   - Measurements are IMMUTABLE in semantics. Corrections create a new
--     row and mark the prior with superseded_at + superseded_by_id. The
--     ranking query selects WHERE superseded_at IS NULL. We never UPDATE
--     measurement values in place — this protects against post-facto
--     disputes ("you changed the number after the reto closed").
--   - Strength tests store raw weight + reps. The 1RM is derived (Epley:
--     1RM ≈ weight × (1 + reps/30)) inside the scoring function. We do
--     NOT denormalise 1RM into the row — keep the source data, recompute
--     on demand.
--   - IR is also computed at query time, not stored. The formula's
--     parameters (cap, weights) live on the challenge row; changing them
--     after measurements exist is rejected at the use-case layer.

BEGIN;

-- ─── challenges (config + lifecycle) ───────────────────────────────────────
CREATE TABLE IF NOT EXISTS challenges (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    name                        TEXT NOT NULL,
    description                 TEXT,
    starts_at                   TIMESTAMPTZ NOT NULL,
    measurement_t0_deadline     TIMESTAMPTZ NOT NULL,
    measurement_t1_start        TIMESTAMPTZ NOT NULL,
    ends_at                     TIMESTAMPTZ NOT NULL,
    status                      TEXT NOT NULL DEFAULT 'draft',
    inscription_fee_cents       INTEGER NOT NULL DEFAULT 0,
    inscription_refundable      BOOLEAN NOT NULL DEFAULT TRUE,
    min_weekly_attendance       INTEGER NOT NULL DEFAULT 3,
    attendance_grace_weeks      INTEGER NOT NULL DEFAULT 2,
    strength_cap_pct            NUMERIC(5,2) NOT NULL DEFAULT 25.00,
    tie_margin_ir               NUMERIC(5,2) NOT NULL DEFAULT 5.00,
    bf_floor_male_pct           NUMERIC(4,2) NOT NULL DEFAULT 6.00,
    bf_floor_female_pct         NUMERIC(4,2) NOT NULL DEFAULT 14.00,

    CONSTRAINT chk_challenges_status CHECK (
        status IN ('draft','open_registration','running','measuring_t1','closed','cancelled')
    ),
    CONSTRAINT chk_challenges_dates CHECK (
        starts_at < measurement_t0_deadline AND
        measurement_t0_deadline <= measurement_t1_start AND
        measurement_t1_start < ends_at
    )
);
CREATE INDEX IF NOT EXISTS idx_challenges_gym ON challenges(gym_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_challenges_sync ON challenges(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_challenges_status ON challenges(gym_id, status) WHERE deleted_at IS NULL;

-- ─── challenge_categories ──────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS challenge_categories (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    challenge_id    UUID NOT NULL REFERENCES challenges(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    name            TEXT NOT NULL,
    sort_order      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_challenge_categories_name
    ON challenge_categories(challenge_id, LOWER(name)) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_challenge_categories_sync ON challenge_categories(gym_id, updated_at);

-- ─── challenge_participants ────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS challenge_participants (
    id              UUID PRIMARY KEY,
    gym_id          UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    challenge_id    UUID NOT NULL REFERENCES challenges(id) ON DELETE RESTRICT,
    member_id       UUID NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    category_id     UUID NOT NULL REFERENCES challenge_categories(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,

    -- Free-form exercise labels chosen at registration (no FK because
    -- the catalog is intentionally small and lives in code, not data).
    exercise_legs           TEXT,
    exercise_push           TEXT,
    exercise_pull           TEXT,

    inscription_fee_paid    BOOLEAN NOT NULL DEFAULT FALSE,
    inscription_paid_at     TIMESTAMPTZ,
    inscription_refunded_at TIMESTAMPTZ,
    status                  TEXT NOT NULL DEFAULT 'registered',
    disqualification_reason TEXT,
    disqualified_at         TIMESTAMPTZ,

    CONSTRAINT chk_participants_status CHECK (
        status IN ('registered','active','disqualified','completed','withdrew')
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_participants_challenge_member
    ON challenge_participants(challenge_id, member_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_participants_sync ON challenge_participants(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_participants_category
    ON challenge_participants(category_id) WHERE deleted_at IS NULL;

-- ─── challenge_measurements ────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS challenge_measurements (
    id                  UUID PRIMARY KEY,
    gym_id              UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    participant_id      UUID NOT NULL REFERENCES challenge_participants(id) ON DELETE RESTRICT,
    version             INTEGER NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,

    moment              TEXT NOT NULL,           -- 't0' | 't1' | 'intermediate'
    measured_at         TIMESTAMPTZ NOT NULL,

    -- Body composition (plicometry-derived).
    body_weight_kg      NUMERIC(5,2) NOT NULL,
    body_fat_pct        NUMERIC(4,2) NOT NULL,

    -- Submaximal strength test: weight × reps per pattern. The 1RM is
    -- derived via Epley at scoring time, not stored.
    legs_weight_kg      NUMERIC(5,2) NOT NULL,
    legs_reps           INTEGER NOT NULL,
    push_weight_kg      NUMERIC(5,2) NOT NULL,
    push_reps           INTEGER NOT NULL,
    pull_weight_kg      NUMERIC(5,2) NOT NULL,
    pull_reps           INTEGER NOT NULL,

    notes               TEXT,
    created_by_user_id  UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    -- Supersession: corrections replace, never mutate. NULL = active.
    superseded_at       TIMESTAMPTZ,
    superseded_by_id    UUID REFERENCES challenge_measurements(id) ON DELETE RESTRICT,

    CONSTRAINT chk_measurements_moment CHECK (moment IN ('t0','t1','intermediate'))
);
-- Partial index for "active measurement per (participant, moment)" — the
-- single query the ranking endpoint hits hardest.
CREATE INDEX IF NOT EXISTS idx_measurements_active
    ON challenge_measurements(participant_id, moment)
    WHERE deleted_at IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_measurements_sync ON challenge_measurements(gym_id, updated_at);

INSERT INTO _migrations (version, name) VALUES (9, '009_challenges')
ON CONFLICT (version) DO NOTHING;

COMMIT;
