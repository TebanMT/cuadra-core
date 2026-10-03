-- SQLite mirror of PostgreSQL/043. JSON remains TEXT locally.
BEGIN;

ALTER TABLE sale_corrections ADD COLUMN idempotency_fingerprint TEXT;
ALTER TABLE sale_corrections ADD COLUMN idempotency_result TEXT
  CHECK(idempotency_result IS NULL OR json_valid(idempotency_result));
ALTER TABLE sale_corrections ADD COLUMN correction_type TEXT NOT NULL DEFAULT 'edit'
  CHECK(correction_type IN ('edit','annul'));
ALTER TABLE sale_corrections ADD COLUMN increase_resolution TEXT
  CHECK(increase_resolution IS NULL OR increase_resolution IN ('pending','already_collected','collect_now'));

UPDATE sale_corrections
SET idempotency_fingerprint = 'legacy:' || id
WHERE idempotency_fingerprint IS NULL OR TRIM(idempotency_fingerprint) = '';

INSERT INTO _migrations(version,name,applied_at)
SELECT 38,'038_sale_correction_idempotency',CAST(strftime('%s','now') AS INTEGER)*1000
WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version=38);
COMMIT;
