-- SQLite mirror of PostgreSQL/039. Money is INTEGER cents and business dates
-- are YYYY-MM-DD. Unknown legacy fields remain NULL/incomplete.
PRAGMA foreign_keys = OFF;
BEGIN;

ALTER TABLE sale_items ADD COLUMN unit_cost_snapshot INTEGER
  CHECK(unit_cost_snapshot IS NULL OR unit_cost_snapshot > 0);
ALTER TABLE sales ADD COLUMN correction_version INTEGER NOT NULL DEFAULT 0
  CHECK(correction_version>=0);

-- Rebuild payments to enforce concept/sign/reference invariants and permit
-- refund methods are real payment rails; there is no fictitious store credit.
CREATE TABLE payments_financial_new (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  folio TEXT NOT NULL,
  member_id TEXT REFERENCES members(id) ON DELETE RESTRICT,
  membership_id TEXT REFERENCES memberships(id) ON DELETE RESTRICT,
  idempotency_key TEXT,
  idempotency_fingerprint TEXT,
  idempotency_result TEXT CHECK(idempotency_result IS NULL OR json_valid(idempotency_result)),
  amount INTEGER NOT NULL,
  recognized_amount INTEGER NOT NULL CHECK(recognized_amount>=0),
  payment_method TEXT NOT NULL CHECK (
    payment_method IN ('cash','transfer','card')
  ),
  concept TEXT NOT NULL CHECK (concept IN ('membership','product','balance_settlement','refund','other')),
  parent_payment_id TEXT REFERENCES payments(id) ON DELETE RESTRICT,
  discount_amount INTEGER NOT NULL DEFAULT 0,
  discount_reason TEXT,
  balance_pending INTEGER NOT NULL DEFAULT 0 CHECK(balance_pending>=0),
  payment_date TEXT NOT NULL,
  notes TEXT,
  operator_id TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  breakdown TEXT,
  CHECK((concept='refund' AND amount<0) OR (concept<>'refund' AND amount>0)),
  CHECK((idempotency_key IS NULL AND idempotency_fingerprint IS NULL AND idempotency_result IS NULL)
     OR (length(trim(idempotency_key)) BETWEEN 1 AND 120 AND length(trim(idempotency_fingerprint))>0)),
  CHECK((concept IN ('refund','balance_settlement') AND parent_payment_id IS NOT NULL)
     OR (concept NOT IN ('refund','balance_settlement') AND parent_payment_id IS NULL))
);
INSERT INTO payments_financial_new(
  id,gym_id,version,created_at,updated_at,deleted_at,synced_at,folio,member_id,membership_id,
  idempotency_key,idempotency_fingerprint,idempotency_result,
  amount,recognized_amount,payment_method,concept,parent_payment_id,discount_amount,discount_reason,
  balance_pending,payment_date,notes,operator_id,breakdown
)
SELECT id,gym_id,version,created_at,updated_at,deleted_at,synced_at,folio,member_id,NULL,NULL,NULL,NULL,
       amount,CASE WHEN concept='refund' THEN 0 ELSE amount END,payment_method,concept,parent_payment_id,discount_amount,discount_reason,
       balance_pending,payment_date,notes,operator_id,breakdown
FROM payments;
DROP TABLE payments;
ALTER TABLE payments_financial_new RENAME TO payments;
CREATE UNIQUE INDEX uq_payments_gym_folio ON payments(gym_id,folio) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX uq_payments_idempotency ON payments(gym_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX idx_payments_member ON payments(member_id,payment_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_payments_gym_date ON payments(gym_id,payment_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_payments_sync ON payments(gym_id,updated_at);
CREATE INDEX idx_payments_concept ON payments(gym_id,concept,payment_date DESC) WHERE deleted_at IS NULL;

-- Rebuild the physical movement journal only to widen its classification enum.
CREATE TABLE cash_movements_financial_new (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id),
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  movement_on TEXT NOT NULL,
  amount INTEGER NOT NULL CHECK(amount>0),
  movement_type TEXT NOT NULL CHECK(movement_type IN ('cash_in','cash_out')),
  reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 200),
  operator_id TEXT NOT NULL REFERENCES users(id),
  expense_id TEXT UNIQUE,
  classification_status TEXT NOT NULL DEFAULT 'unclassified'
    CHECK(classification_status IN ('unclassified','expense','inventory_purchase','non_operating'))
);
INSERT INTO cash_movements_financial_new
SELECT id,gym_id,version,created_at,updated_at,deleted_at,synced_at,movement_on,
       amount,movement_type,reason,operator_id,expense_id,classification_status
FROM cash_movements;
DROP TABLE cash_movements;
ALTER TABLE cash_movements_financial_new RENAME TO cash_movements;
CREATE INDEX idx_cash_movements_gym_day
  ON cash_movements(gym_id,movement_on,id) WHERE deleted_at IS NULL;

CREATE TABLE inventory_purchases (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  stock_movement_id TEXT NOT NULL UNIQUE REFERENCES stock_movements(id) ON DELETE RESTRICT,
  product_id TEXT NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
  quantity INTEGER NOT NULL CHECK(quantity>0),
  unit_cost INTEGER,
  total_amount INTEGER,
  status TEXT NOT NULL CHECK(status IN ('unpaid','paid','legacy_incomplete')),
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
    OR (status='unpaid' AND paid_on IS NULL AND payment_method IS NULL AND paid_from IS NULL
      AND cash_movement_id IS NULL
      AND (unit_cost IS NULL OR unit_cost>0) AND (total_amount IS NULL OR total_amount>0))
    OR status='legacy_incomplete'
  ),
  CHECK(paid_from<>'cash_drawer' OR payment_method='cash'),
  CHECK((status='paid' AND paid_from='cash_drawer' AND cash_movement_id IS NOT NULL)
     OR (paid_from IS NOT 'cash_drawer' AND cash_movement_id IS NULL)
     OR status='legacy_incomplete')
);

CREATE TABLE refunds (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  root_payment_id TEXT NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
  refund_payment_id TEXT UNIQUE REFERENCES payments(id) ON DELETE RESTRICT,
  sale_id TEXT REFERENCES sales(id) ON DELETE RESTRICT,
  amount INTEGER NOT NULL CHECK(amount>=0),
  method TEXT CHECK(method IN ('cash','transfer','card')),
  refunded_on TEXT NOT NULL,
  reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 200),
  kind TEXT NOT NULL DEFAULT 'revenue_refund'
    CHECK(kind IN ('revenue_refund','overcollection_settlement')),
  balance_cancelled INTEGER NOT NULL DEFAULT 0 CHECK(balance_cancelled>=0),
  legacy_incomplete INTEGER NOT NULL DEFAULT 0,
  correction_id TEXT,
  idempotency_key TEXT NOT NULL,
  created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  CHECK((amount>0 AND method IS NOT NULL) OR
        (amount=0 AND balance_cancelled>0 AND method IS NULL)),
  UNIQUE(gym_id,idempotency_key)
);

CREATE TABLE refund_items (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  refund_id TEXT NOT NULL REFERENCES refunds(id) ON DELETE RESTRICT,
  sale_item_id TEXT NOT NULL REFERENCES sale_items(id) ON DELETE RESTRICT,
  quantity INTEGER NOT NULL CHECK(quantity>0),
  amount INTEGER NOT NULL CHECK(amount>0),
  disposition TEXT NOT NULL CHECK(disposition IN ('returned_to_stock','damaged','not_returned')),
  UNIQUE(refund_id,sale_item_id)
);

CREATE TABLE sale_corrections (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  sale_id TEXT NOT NULL REFERENCES sales(id) ON DELETE RESTRICT,
  expected_sale_version INTEGER NOT NULL CHECK(expected_sale_version>=0),
  reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 200),
  money_resolution TEXT NOT NULL CHECK(money_resolution IN ('record_only','refund_excess','refund_pending')),
  monetary_delta INTEGER NOT NULL,
  before_snapshot TEXT NOT NULL CHECK(json_valid(before_snapshot)),
  after_snapshot TEXT NOT NULL CHECK(json_valid(after_snapshot)),
  refund_id TEXT REFERENCES refunds(id) ON DELETE RESTRICT,
  idempotency_key TEXT NOT NULL,
  created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  UNIQUE(gym_id,idempotency_key)
);

-- Cyclic optional link is added after both tables exist.
CREATE TRIGGER refunds_correction_fk_insert
BEFORE INSERT ON refunds
WHEN NEW.correction_id IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM sale_corrections WHERE id=NEW.correction_id)
BEGIN SELECT RAISE(ABORT,'invalid refund correction'); END;

INSERT INTO refunds(
  id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
  root_payment_id,refund_payment_id,sale_id,amount,method,refunded_on,reason,
  balance_cancelled,legacy_incomplete,idempotency_key,created_by
)
SELECT p.id,p.gym_id,p.version,p.created_at,p.updated_at,p.deleted_at,p.synced_at,
       p.parent_payment_id,p.id,s.id,ABS(p.amount),p.payment_method,p.payment_date,
       COALESCE(NULLIF(TRIM(p.notes),''),'Devolución legacy'),0,1,
       'legacy:' || p.id,p.operator_id
FROM payments p
LEFT JOIN sales s ON s.payment_id=p.parent_payment_id AND s.deleted_at IS NULL
WHERE p.concept='refund' AND p.parent_payment_id IS NOT NULL
ON CONFLICT(refund_payment_id) DO NOTHING;

-- SQLite cannot add a CHECK to stock_movements in place. Equivalent triggers
-- protect every new/updated financial cost without rewriting legacy rows.
CREATE TRIGGER stock_movements_positive_cost_insert
BEFORE INSERT ON stock_movements
WHEN NEW.cost IS NOT NULL AND NEW.cost<=0
BEGIN SELECT RAISE(ABORT,'stock movement cost must be positive'); END;
CREATE TRIGGER stock_movements_positive_cost_update
BEFORE UPDATE OF cost ON stock_movements
WHEN NEW.cost IS NOT NULL AND NEW.cost<=0
BEGIN SELECT RAISE(ABORT,'stock movement cost must be positive'); END;

CREATE INDEX idx_inventory_purchases_gym_paid
  ON inventory_purchases(gym_id,paid_on,id) WHERE deleted_at IS NULL AND status='paid';
CREATE INDEX idx_inventory_purchases_product
  ON inventory_purchases(product_id,created_at,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_inventory_purchases_sync ON inventory_purchases(gym_id,updated_at);
CREATE INDEX idx_refunds_root ON refunds(root_payment_id,refunded_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refunds_correction_kind ON refunds(correction_id,kind,refunded_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refunds_gym_date ON refunds(gym_id,refunded_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refunds_sync ON refunds(gym_id,updated_at);
CREATE INDEX idx_refund_items_sale_item ON refund_items(sale_item_id,created_at,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refund_items_sync ON refund_items(gym_id,updated_at);
CREATE INDEX idx_sale_corrections_sale ON sale_corrections(sale_id,created_at,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_sale_corrections_sync ON sale_corrections(gym_id,updated_at);

INSERT INTO _migrations(version,name,applied_at)
SELECT 34,'034_financial_integrity',CAST(strftime('%s','now') AS INTEGER)*1000
WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version=34);
COMMIT;
PRAGMA foreign_keys = ON;
