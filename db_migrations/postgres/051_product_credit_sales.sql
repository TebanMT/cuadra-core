-- A product sale records its debt even when no money is collected.
BEGIN;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_sign;
ALTER TABLE payments ADD CONSTRAINT chk_payments_sign CHECK (
 (concept='refund' AND amount<0) OR
 (concept='product' AND amount>=0) OR
 (concept NOT IN ('refund','product') AND amount>0)
);
INSERT INTO _migrations(version,name,applied_at) VALUES(51,'051_product_credit_sales',NOW());
COMMIT;
