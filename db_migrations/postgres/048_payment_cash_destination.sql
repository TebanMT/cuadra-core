BEGIN;
ALTER TABLE payments ADD COLUMN cash_destination TEXT NOT NULL DEFAULT 'cash_drawer' CHECK (cash_destination IN ('cash_drawer','gym_fund'));
INSERT INTO _migrations(version,name)
VALUES(48,'048_payment_cash_destination')
ON CONFLICT(version) DO NOTHING;
COMMIT;
