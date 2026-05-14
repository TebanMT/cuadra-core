-- Migration 006 — challenges (retos) — sidecar mirror of postgres/009.
--
-- Local copy of the four challenges tables for offline operation. The
-- nutricionista captures mediciones in the gym's laptop while seated next
-- to the participant; that flow can't depend on Hetzner being reachable.
-- The sync agent pushes the upserts; the projector applies them cloud-side.
--
-- SQLite type mapping convention (same as the rest of Tinta):
--   - TIMESTAMPTZ -> INTEGER (unix epoch ms)
--   - BOOLEAN     -> INTEGER (0/1)
--   - NUMERIC     -> REAL (we don't need exact decimal precision for
--                    these fields and SQLite has no NUMERIC type anyway)
--   - synced_at column added for the sync queue's outbound bookkeeping.

BEGIN;

CREATE TABLE IF NOT EXISTS challenges (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    name                        TEXT NOT NULL,
    description                 TEXT,
    starts_at                   INTEGER NOT NULL,
    measurement_t0_deadline     INTEGER NOT NULL,
    measurement_t1_start        INTEGER NOT NULL,
    ends_at                     INTEGER NOT NULL,
    status                      TEXT NOT NULL DEFAULT 'draft'
                                CHECK (status IN ('draft','open_registration','running','measuring_t1','closed','cancelled')),
    inscription_fee_cents       INTEGER NOT NULL DEFAULT 0,
    inscription_refundable      INTEGER NOT NULL DEFAULT 1,
    min_weekly_attendance       INTEGER NOT NULL DEFAULT 3,
    attendance_grace_weeks      INTEGER NOT NULL DEFAULT 2,
    strength_cap_pct            REAL NOT NULL DEFAULT 25.0,
    tie_margin_ir               REAL NOT NULL DEFAULT 5.0,
    bf_floor_male_pct           REAL NOT NULL DEFAULT 6.0,
    bf_floor_female_pct         REAL NOT NULL DEFAULT 14.0
);
CREATE INDEX IF NOT EXISTS idx_challenges_gym ON challenges(gym_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_challenges_sync ON challenges(gym_id, updated_at);

CREATE TABLE IF NOT EXISTS challenge_categories (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    challenge_id    TEXT NOT NULL REFERENCES challenges(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    name            TEXT NOT NULL,
    sort_order      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_challenge_categories_name
    ON challenge_categories(challenge_id, name COLLATE NOCASE) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_challenge_categories_sync ON challenge_categories(gym_id, updated_at);

CREATE TABLE IF NOT EXISTS challenge_participants (
    id              TEXT PRIMARY KEY,
    gym_id          TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    challenge_id    TEXT NOT NULL REFERENCES challenges(id) ON DELETE RESTRICT,
    member_id       TEXT NOT NULL REFERENCES members(id) ON DELETE RESTRICT,
    category_id     TEXT NOT NULL REFERENCES challenge_categories(id) ON DELETE RESTRICT,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    synced_at       INTEGER,

    exercise_legs           TEXT,
    exercise_push           TEXT,
    exercise_pull           TEXT,
    inscription_fee_paid    INTEGER NOT NULL DEFAULT 0,
    inscription_paid_at     INTEGER,
    inscription_refunded_at INTEGER,
    status                  TEXT NOT NULL DEFAULT 'registered'
                            CHECK (status IN ('registered','active','disqualified','completed','withdrew')),
    disqualification_reason TEXT,
    disqualified_at         INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_participants_challenge_member
    ON challenge_participants(challenge_id, member_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_participants_sync ON challenge_participants(gym_id, updated_at);
CREATE INDEX IF NOT EXISTS idx_participants_category
    ON challenge_participants(category_id) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS challenge_measurements (
    id                  TEXT PRIMARY KEY,
    gym_id              TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
    participant_id      TEXT NOT NULL REFERENCES challenge_participants(id) ON DELETE RESTRICT,
    version             INTEGER NOT NULL DEFAULT 1,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    deleted_at          INTEGER,
    synced_at           INTEGER,

    moment              TEXT NOT NULL CHECK (moment IN ('t0','t1','intermediate')),
    measured_at         INTEGER NOT NULL,
    body_weight_kg      REAL NOT NULL,
    body_fat_pct        REAL NOT NULL,
    legs_weight_kg      REAL NOT NULL,
    legs_reps           INTEGER NOT NULL,
    push_weight_kg      REAL NOT NULL,
    push_reps           INTEGER NOT NULL,
    pull_weight_kg      REAL NOT NULL,
    pull_reps           INTEGER NOT NULL,
    notes               TEXT,
    created_by_user_id  TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    superseded_at       INTEGER,
    superseded_by_id    TEXT REFERENCES challenge_measurements(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_measurements_active
    ON challenge_measurements(participant_id, moment)
    WHERE deleted_at IS NULL AND superseded_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_measurements_sync ON challenge_measurements(gym_id, updated_at);

INSERT INTO _migrations (version, name, applied_at)
SELECT 6, '006_challenges', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS (SELECT 1 FROM _migrations WHERE version = 6);

COMMIT;
