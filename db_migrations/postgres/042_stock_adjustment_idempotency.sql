-- Every manual stock adjustment is an idempotent command. This is especially
-- important offline: a timeout/retry must never add or remove stock twice.
BEGIN;

ALTER TABLE stock_movements ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
ALTER TABLE stock_movements ADD COLUMN IF NOT EXISTS idempotency_fingerprint TEXT;
ALTER TABLE stock_movements ADD COLUMN IF NOT EXISTS idempotency_result JSONB;

ALTER TABLE stock_movements DROP CONSTRAINT IF EXISTS chk_stock_movements_idempotency_shape;
ALTER TABLE stock_movements ADD CONSTRAINT chk_stock_movements_idempotency_shape CHECK (
  (idempotency_key IS NULL AND idempotency_fingerprint IS NULL AND idempotency_result IS NULL)
  OR (
    char_length(BTRIM(idempotency_key)) BETWEEN 1 AND 120
    AND char_length(BTRIM(idempotency_fingerprint)) > 0
    AND idempotency_result IS NOT NULL
  )
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_stock_movements_gym_idempotency
  ON stock_movements(gym_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;

INSERT INTO _migrations(version,name) VALUES(42,'042_stock_adjustment_idempotency')
ON CONFLICT(version) DO NOTHING;
COMMIT;
