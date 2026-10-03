-- Audited administrative corrections for non-product income payments.
-- The payment row remains the canonical read model; every exceptional
-- rewrite has immutable before/after evidence and exact idempotent replay.
BEGIN;

CREATE TABLE payment_corrections (
  id UUID PRIMARY KEY,
  gym_id UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1 CHECK(version>0),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  deleted_at TIMESTAMPTZ,
  payment_id UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
  expected_payment_version INTEGER NOT NULL CHECK(expected_payment_version>0),
  reason VARCHAR(200) NOT NULL CHECK(length(btrim(reason))>=3),
  before_snapshot JSONB NOT NULL CHECK(jsonb_typeof(before_snapshot)='object'),
  after_snapshot JSONB NOT NULL CHECK(jsonb_typeof(after_snapshot)='object'),
  idempotency_key VARCHAR(120) NOT NULL,
  idempotency_fingerprint VARCHAR(128) NOT NULL,
  idempotency_result JSONB,
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  CONSTRAINT uq_payment_corrections_idempotency UNIQUE(gym_id,idempotency_key),
  CONSTRAINT chk_payment_corrections_idempotency_shape CHECK (
    length(btrim(idempotency_key)) BETWEEN 1 AND 120
    AND length(btrim(idempotency_fingerprint)) > 0
  )
);

CREATE INDEX idx_payment_corrections_payment
  ON payment_corrections(gym_id,payment_id,created_at,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_payment_corrections_sync
  ON payment_corrections(gym_id,updated_at);

INSERT INTO _migrations(version,name)
VALUES(45,'045_payment_corrections')
ON CONFLICT(version) DO NOTHING;
COMMIT;
