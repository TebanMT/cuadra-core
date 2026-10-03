-- A manual cash-in is physical float/contribution, never an expense or an
-- inventory purchase. Business income is represented by payments (concept
-- other), not by reclassifying the cash journal.
BEGIN;

UPDATE cash_movements
SET classification_status='non_operating'
WHERE movement_type='cash_in'
  AND classification_status='unclassified'
  AND expense_id IS NULL;

UPDATE sync_entities se
SET payload = jsonb_set(se.payload, '{classification_status}', '"non_operating"'::jsonb, TRUE)
FROM cash_movements cm
WHERE se.entity_type='cash_movements'
  AND se.gym_id=cm.gym_id AND se.entity_id=cm.id
  AND cm.movement_type='cash_in'
  AND cm.classification_status='non_operating'
  AND cm.expense_id IS NULL;

ALTER TABLE cash_movements
  DROP CONSTRAINT IF EXISTS chk_cash_movements_cash_in_non_operating;
-- NOT VALID preserves any historical bad row so an owner can repair it with
-- the audited unclassification flow. PostgreSQL enforces it on every live new
-- or updated row while allowing source-owned corrections to write tombstones.
ALTER TABLE cash_movements ADD CONSTRAINT chk_cash_movements_cash_in_non_operating
  CHECK (deleted_at IS NOT NULL OR movement_type<>'cash_in' OR
    (classification_status='non_operating' AND expense_id IS NULL)) NOT VALID;

INSERT INTO _migrations(version,name) VALUES(46,'046_cash_in_non_operating')
ON CONFLICT(version) DO NOTHING;
COMMIT;
