-- SQLite mirror of PostgreSQL/046. Triggers provide a forward write barrier
-- without discarding a possible historical invalid row. A live row can only
-- be repaired to the valid non-operating state; source-owned corrections may
-- also preserve it as an immutable tombstone.
BEGIN;

UPDATE cash_movements
SET classification_status='non_operating'
WHERE movement_type='cash_in'
  AND classification_status='unclassified'
  AND expense_id IS NULL;

UPDATE sync_queue
SET payload=json_set(payload,'$.classification_status','non_operating')
WHERE entity_type='cash_movements' AND synced_at IS NULL AND json_valid(payload)
  AND json_extract(payload,'$.movement_type')='cash_in'
  AND json_extract(payload,'$.classification_status')='unclassified'
  AND json_extract(payload,'$.expense_id') IS NULL;

CREATE TRIGGER IF NOT EXISTS trg_cash_movements_cash_in_classification_insert_041
BEFORE INSERT ON cash_movements
WHEN NEW.deleted_at IS NULL
 AND NEW.movement_type='cash_in'
 AND (NEW.classification_status<>'non_operating' OR NEW.expense_id IS NOT NULL)
BEGIN
  SELECT RAISE(ABORT,'cash_in must be non_operating and cannot link an expense');
END;

CREATE TRIGGER IF NOT EXISTS trg_cash_movements_cash_in_classification_update_041
BEFORE UPDATE OF movement_type,classification_status,expense_id,deleted_at ON cash_movements
WHEN NEW.deleted_at IS NULL
 AND NEW.movement_type='cash_in'
 AND (NEW.classification_status<>'non_operating' OR NEW.expense_id IS NOT NULL)
BEGIN
  SELECT RAISE(ABORT,'cash_in must be non_operating and cannot link an expense');
END;

INSERT INTO _migrations(version,name,applied_at)
SELECT 41,'041_cash_in_non_operating',CAST(strftime('%s','now') AS INTEGER)*1000
WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version=41);
COMMIT;
