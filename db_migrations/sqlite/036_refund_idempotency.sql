-- Exact semantic idempotency for refund commands. JSON is stored as TEXT in
-- SQLite and remains JSONB on PostgreSQL.
BEGIN;

ALTER TABLE refunds ADD COLUMN idempotency_fingerprint TEXT;
ALTER TABLE refunds ADD COLUMN idempotency_result TEXT
  CHECK(idempotency_result IS NULL OR json_valid(idempotency_result));

UPDATE refunds
SET idempotency_fingerprint = 'legacy:' || id
WHERE idempotency_fingerprint IS NULL OR TRIM(idempotency_fingerprint) = '';

INSERT INTO _migrations(version,name,applied_at)
SELECT 36,'036_refund_idempotency',CAST(strftime('%s','now') AS INTEGER)*1000
WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version=36);
COMMIT;
