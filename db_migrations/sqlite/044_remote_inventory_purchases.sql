-- Remote payments and immutable physical receipts; mirror of Postgres 049.
PRAGMA foreign_keys = OFF;
BEGIN;

ALTER TABLE inventory_purchases RENAME TO inventory_purchases_legacy_044;

CREATE TABLE inventory_purchases (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  stock_movement_id TEXT UNIQUE REFERENCES stock_movements(id) ON DELETE RESTRICT,
  product_id TEXT NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
  quantity INTEGER NOT NULL CHECK(quantity>0),
  unit_cost INTEGER,
  total_amount INTEGER,
  status TEXT NOT NULL CHECK(status IN ('unpaid','paid','legacy_incomplete','annulled')),
  paid_on TEXT,
  payment_method TEXT CHECK(payment_method IS NULL OR payment_method IN ('cash','transfer','card')),
  paid_from TEXT CHECK(paid_from IS NULL OR paid_from IN ('cash_drawer','gym_fund','external')),
  cash_movement_id TEXT UNIQUE REFERENCES cash_movements(id) ON DELETE RESTRICT,
  idempotency_key TEXT NOT NULL,
  created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  UNIQUE(gym_id,idempotency_key),
  CHECK(
    (status='paid' AND unit_cost>0 AND total_amount>0
      AND total_amount=unit_cost*quantity
      AND paid_on IS NOT NULL AND payment_method IS NOT NULL AND paid_from IS NOT NULL)
    OR (status='unpaid'
      AND paid_on IS NULL AND payment_method IS NULL AND paid_from IS NULL
      AND cash_movement_id IS NULL
      AND (unit_cost IS NULL OR unit_cost>0) AND (total_amount IS NULL OR total_amount>0))
    OR (status='annulled'
      AND paid_on IS NULL AND payment_method IS NULL AND paid_from IS NULL
      AND cash_movement_id IS NULL AND unit_cost>0 AND total_amount>0)
    OR status='legacy_incomplete'
  ),
  CHECK(paid_from<>'cash_drawer' OR payment_method='cash'),
  CHECK((status='paid' AND paid_from='cash_drawer' AND cash_movement_id IS NOT NULL)
     OR (paid_from IS NOT 'cash_drawer' AND cash_movement_id IS NULL)
     OR status='legacy_incomplete')
);

INSERT INTO inventory_purchases(
  id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
  stock_movement_id,product_id,quantity,unit_cost,total_amount,status,paid_on,
  payment_method,paid_from,cash_movement_id,idempotency_key,created_by)
SELECT
  id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
  stock_movement_id,product_id,quantity,unit_cost,total_amount,status,paid_on,
  payment_method,paid_from,cash_movement_id,idempotency_key,created_by
FROM inventory_purchases_legacy_044;

DROP TABLE inventory_purchases_legacy_044;

CREATE INDEX idx_inventory_purchases_gym_paid
  ON inventory_purchases(gym_id,paid_on,id) WHERE deleted_at IS NULL AND status='paid';
CREATE INDEX idx_inventory_purchases_product
  ON inventory_purchases(product_id,created_at,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_inventory_purchases_sync
  ON inventory_purchases(gym_id,updated_at);

ALTER TABLE products ADD COLUMN stock_base INTEGER;
UPDATE products SET stock_base=stock;
CREATE TABLE inventory_purchase_receipts (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id),
  version INTEGER NOT NULL DEFAULT 1 CHECK(version=1),
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  deleted_at INTEGER CHECK(deleted_at IS NULL), synced_at INTEGER,
  purchase_id TEXT NOT NULL UNIQUE REFERENCES inventory_purchases(id),
  product_id TEXT NOT NULL REFERENCES products(id),
  quantity INTEGER NOT NULL CHECK(quantity > 0),
  unit_cost INTEGER NOT NULL CHECK(unit_cost > 0),
  received_by TEXT NOT NULL REFERENCES users(id),
  CHECK(id=purchase_id)
);
CREATE INDEX idx_purchase_receipts_product ON inventory_purchase_receipts(gym_id,product_id);
-- Queued snapshots predate every remote receipt and therefore include none.
UPDATE sync_queue SET payload=json_set(payload,'$.stock_base',json_extract(payload,'$.stock'))
WHERE entity_type='products' AND json_extract(payload,'$.stock_base') IS NULL;
INSERT INTO _migrations(version,name,applied_at)
SELECT 44,'044_remote_inventory_purchases',CAST(strftime('%s','now') AS INTEGER)*1000
WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version=44);
COMMIT;
PRAGMA foreign_keys = ON;
