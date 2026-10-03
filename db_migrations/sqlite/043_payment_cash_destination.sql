BEGIN;
ALTER TABLE payments ADD COLUMN cash_destination TEXT NOT NULL DEFAULT 'cash_drawer' CHECK (cash_destination IN ('cash_drawer','gym_fund'));
INSERT INTO _migrations(version,name,applied_at)
VALUES(43,'043_payment_cash_destination',CAST(strftime('%s','now') AS INTEGER)*1000);
COMMIT;
