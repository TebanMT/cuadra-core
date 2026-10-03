-- SQLite mirror of PostgreSQL/042. JSON remains TEXT locally.
BEGIN;

ALTER TABLE stock_movements ADD COLUMN idempotency_key TEXT;
ALTER TABLE stock_movements ADD COLUMN idempotency_fingerprint TEXT;
ALTER TABLE stock_movements ADD COLUMN idempotency_result TEXT
  CHECK(idempotency_result IS NULL OR json_valid(idempotency_result));

CREATE UNIQUE INDEX IF NOT EXISTS uq_stock_movements_gym_idempotency
  ON stock_movements(gym_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;

INSERT INTO _migrations(version,name,applied_at)
SELECT 37,'037_stock_adjustment_idempotency',CAST(strftime('%s','now') AS INTEGER)*1000
WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version=37);
COMMIT;
