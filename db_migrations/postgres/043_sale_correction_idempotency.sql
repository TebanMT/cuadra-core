-- A sale-correction retry must compare the complete semantic command and
-- replay the exact committed money effect. Historical corrections receive a
-- non-matching legacy fingerprint so their keys cannot be guessed as equal.
BEGIN;

ALTER TABLE sale_corrections
  ADD COLUMN IF NOT EXISTS idempotency_fingerprint TEXT;
ALTER TABLE sale_corrections
  ADD COLUMN IF NOT EXISTS idempotency_result JSONB;
ALTER TABLE sale_corrections
  ADD COLUMN IF NOT EXISTS correction_type TEXT NOT NULL DEFAULT 'edit';
ALTER TABLE sale_corrections
  ADD COLUMN IF NOT EXISTS increase_resolution TEXT;

UPDATE sale_corrections
SET idempotency_fingerprint = 'legacy:' || id::text
WHERE idempotency_fingerprint IS NULL OR BTRIM(idempotency_fingerprint) = '';

ALTER TABLE sale_corrections
  ALTER COLUMN idempotency_fingerprint SET NOT NULL;
ALTER TABLE sale_corrections
  DROP CONSTRAINT IF EXISTS chk_sale_corrections_idempotency_shape;
ALTER TABLE sale_corrections
  ADD CONSTRAINT chk_sale_corrections_idempotency_shape CHECK (
    char_length(BTRIM(idempotency_key)) BETWEEN 1 AND 120
    AND char_length(BTRIM(idempotency_fingerprint)) > 0
  );
ALTER TABLE sale_corrections
  DROP CONSTRAINT IF EXISTS chk_sale_corrections_extended_resolution;
ALTER TABLE sale_corrections
  ADD CONSTRAINT chk_sale_corrections_extended_resolution CHECK (
    correction_type IN ('edit','annul')
    AND (increase_resolution IS NULL OR increase_resolution IN ('pending','already_collected','collect_now'))
  );

INSERT INTO _migrations(version,name)
VALUES(43,'043_sale_correction_idempotency')
ON CONFLICT(version) DO NOTHING;
COMMIT;
