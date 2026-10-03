-- A refund retry must compare the complete semantic request and replay the
-- exact committed response. The key alone cannot distinguish changed amount,
-- drawer, date, membership flag or returned product lines.
BEGIN;

ALTER TABLE refunds ADD COLUMN IF NOT EXISTS idempotency_fingerprint TEXT;
ALTER TABLE refunds ADD COLUMN IF NOT EXISTS idempotency_result JSONB;

-- Historical aggregates predate exact command replay. Keep them readable but
-- make any reuse of their old key conflict instead of guessing equivalence.
UPDATE refunds
SET idempotency_fingerprint = 'legacy:' || id::text
WHERE idempotency_fingerprint IS NULL OR BTRIM(idempotency_fingerprint) = '';

ALTER TABLE refunds ALTER COLUMN idempotency_fingerprint SET NOT NULL;
ALTER TABLE refunds DROP CONSTRAINT IF EXISTS chk_refunds_idempotency_shape;
ALTER TABLE refunds ADD CONSTRAINT chk_refunds_idempotency_shape CHECK (
  char_length(BTRIM(idempotency_key)) BETWEEN 1 AND 120
  AND char_length(BTRIM(idempotency_fingerprint)) > 0
);

INSERT INTO _migrations(version,name) VALUES(41,'041_refund_idempotency')
ON CONFLICT(version) DO NOTHING;
COMMIT;
