-- Distinguish how a payment was made from which pool of money paid it.
BEGIN;

ALTER TABLE expenses ADD COLUMN paid_from TEXT NOT NULL DEFAULT 'gym_fund'
  CHECK(paid_from IN ('cash_register','gym_fund','external'));
UPDATE expenses SET paid_from='cash_register' WHERE payment_method='cash';
UPDATE sync_queue SET payload=json_set(payload, '$.paid_from',
  CASE WHEN json_extract(payload,'$.payment_method')='cash' THEN 'cash_register' ELSE 'gym_fund' END)
WHERE entity_type='expenses' AND synced_at IS NULL AND json_valid(payload);

INSERT INTO _migrations(version,name,applied_at)
 SELECT 33,'033_simple_financial_model',CAST(strftime('%s','now') AS INTEGER)*1000
 WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version=33);
COMMIT;
