-- SQLite mirror of PG045. JSON snapshots use TEXT guarded by json_valid;
-- sync metadata is local-only and follows the rest of the sidecar schema.
BEGIN;

CREATE TABLE payment_corrections (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1 CHECK(version>0),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  payment_id TEXT NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
  expected_payment_version INTEGER NOT NULL CHECK(expected_payment_version>0),
  reason TEXT NOT NULL CHECK(length(trim(reason)) BETWEEN 3 AND 200),
  before_snapshot TEXT NOT NULL CHECK(json_valid(before_snapshot) AND json_type(before_snapshot)='object'),
  after_snapshot TEXT NOT NULL CHECK(json_valid(after_snapshot) AND json_type(after_snapshot)='object'),
  idempotency_key TEXT NOT NULL CHECK(length(trim(idempotency_key)) BETWEEN 1 AND 120),
  idempotency_fingerprint TEXT NOT NULL CHECK(length(trim(idempotency_fingerprint)) BETWEEN 1 AND 128),
  idempotency_result TEXT CHECK(idempotency_result IS NULL OR json_valid(idempotency_result)),
  created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  UNIQUE(gym_id,idempotency_key)
);

CREATE INDEX idx_payment_corrections_payment
  ON payment_corrections(gym_id,payment_id,created_at,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_payment_corrections_sync
  ON payment_corrections(gym_id,updated_at);

INSERT INTO _migrations(version,name,applied_at)
SELECT 40,'040_payment_corrections',CAST(strftime('%s','now') AS INTEGER)*1000
WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version=40);
COMMIT;
