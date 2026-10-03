-- Distinguish how a payment was made from which pool of money paid it.
BEGIN;

ALTER TABLE expenses ADD COLUMN IF NOT EXISTS paid_from TEXT;
UPDATE expenses
SET paid_from = CASE WHEN payment_method = 'cash' THEN 'cash_register' ELSE 'gym_fund' END
WHERE paid_from IS NULL OR paid_from = '';
ALTER TABLE expenses ALTER COLUMN paid_from SET DEFAULT 'gym_fund';
ALTER TABLE expenses ALTER COLUMN paid_from SET NOT NULL;
ALTER TABLE expenses DROP CONSTRAINT IF EXISTS chk_expenses_paid_from;
ALTER TABLE expenses ADD CONSTRAINT chk_expenses_paid_from
  CHECK (paid_from IN ('cash_register','gym_fund','external'));
ALTER TABLE expenses DROP CONSTRAINT IF EXISTS chk_expenses_cash_register_method;
ALTER TABLE expenses ADD CONSTRAINT chk_expenses_cash_register_method
  CHECK (paid_from <> 'cash_register' OR payment_method = 'cash');

UPDATE sync_entities se
SET payload = se.payload || jsonb_build_object('paid_from', e.paid_from)
FROM expenses e
WHERE se.entity_type = 'expenses' AND se.entity_id = e.id AND se.gym_id = e.gym_id;

INSERT INTO _migrations(version,name) VALUES(38,'038_simple_financial_model')
ON CONFLICT(version) DO NOTHING;
COMMIT;
