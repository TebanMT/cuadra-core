-- Explicit purchase corrections preserve the original stock journal while
-- allowing an owner to annul an unpaid receipt. "annulled" is historical,
-- never an outstanding or paid outflow.
BEGIN;

ALTER TABLE inventory_purchases
  DROP CONSTRAINT IF EXISTS inventory_purchases_status_check;
ALTER TABLE inventory_purchases
  DROP CONSTRAINT IF EXISTS chk_inventory_purchases_status;
ALTER TABLE inventory_purchases
  ADD CONSTRAINT chk_inventory_purchases_status
  CHECK (status IN ('unpaid','paid','legacy_incomplete','annulled'));

ALTER TABLE inventory_purchases
  DROP CONSTRAINT IF EXISTS chk_inventory_purchases_paid_shape;
ALTER TABLE inventory_purchases
  ADD CONSTRAINT chk_inventory_purchases_paid_shape CHECK (
    (status = 'paid'
      AND unit_cost > 0 AND total_amount > 0
      AND total_amount = ROUND(unit_cost * quantity, 2)
      AND paid_on IS NOT NULL AND payment_method IS NOT NULL AND paid_from IS NOT NULL)
    OR (status = 'unpaid'
      AND paid_on IS NULL AND payment_method IS NULL AND paid_from IS NULL
      AND cash_movement_id IS NULL
      AND (unit_cost IS NULL OR unit_cost > 0)
      AND (total_amount IS NULL OR total_amount > 0))
    OR (status = 'annulled'
      AND paid_on IS NULL AND payment_method IS NULL AND paid_from IS NULL
      AND cash_movement_id IS NULL
      AND unit_cost > 0 AND total_amount > 0)
    OR status = 'legacy_incomplete'
  );

INSERT INTO _migrations(version,name)
VALUES(44,'044_inventory_purchase_corrections')
ON CONFLICT(version) DO NOTHING;
COMMIT;
