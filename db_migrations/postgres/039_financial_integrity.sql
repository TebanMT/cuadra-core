-- ADR-011: canonical business result, paid inventory purchases, aggregate
-- refunds, immutable sale cost snapshots and auditable sale corrections.
-- Legacy uncertainty is preserved as such; this migration never invents cost,
-- payment source or returned merchandise.
BEGIN;

ALTER TABLE sale_items
  ADD COLUMN IF NOT EXISTS unit_cost_snapshot NUMERIC(12,2);
ALTER TABLE sale_items DROP CONSTRAINT IF EXISTS chk_sale_items_unit_cost_snapshot;
ALTER TABLE sale_items ADD CONSTRAINT chk_sale_items_unit_cost_snapshot
  CHECK (unit_cost_snapshot IS NULL OR unit_cost_snapshot > 0);

-- Kept separate from the generic row version: it is the optimistic token the
-- correction API exposes and only advances for audited sale corrections.
ALTER TABLE sales ADD COLUMN IF NOT EXISTS correction_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sales DROP CONSTRAINT IF EXISTS chk_sales_correction_version;
ALTER TABLE sales ADD CONSTRAINT chk_sales_correction_version CHECK (correction_version >= 0);

ALTER TABLE payments ADD COLUMN IF NOT EXISTS recognized_amount NUMERIC(12,2);
ALTER TABLE payments ADD COLUMN IF NOT EXISTS membership_id UUID
  REFERENCES memberships(id) ON DELETE RESTRICT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS idempotency_fingerprint TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS idempotency_result JSONB;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_idempotency_shape;
ALTER TABLE payments ADD CONSTRAINT chk_payments_idempotency_shape CHECK (
  (idempotency_key IS NULL AND idempotency_fingerprint IS NULL AND idempotency_result IS NULL)
  OR (char_length(BTRIM(idempotency_key)) BETWEEN 1 AND 120
      AND char_length(BTRIM(idempotency_fingerprint)) > 0)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_payments_idempotency
  ON payments(gym_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;
UPDATE payments SET recognized_amount=CASE WHEN concept='refund' THEN 0 ELSE amount END
WHERE recognized_amount IS NULL;
ALTER TABLE payments ALTER COLUMN recognized_amount SET DEFAULT 0;
ALTER TABLE payments ALTER COLUMN recognized_amount SET NOT NULL;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_recognized_amount;
ALTER TABLE payments ADD CONSTRAINT chk_payments_recognized_amount
  CHECK (recognized_amount >= 0);

-- A paid purchase is a financial event linked 1:1 to the inventory receipt.
-- Unpaid receipts are useful operationally but do not enter period outflows.
CREATE TABLE inventory_purchases (
  id UUID PRIMARY KEY,
  gym_id UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  deleted_at TIMESTAMPTZ,
  stock_movement_id UUID NOT NULL REFERENCES stock_movements(id) ON DELETE RESTRICT,
  product_id UUID NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
  quantity INTEGER NOT NULL CHECK (quantity > 0),
  unit_cost NUMERIC(12,2),
  total_amount NUMERIC(12,2),
  status TEXT NOT NULL CHECK (status IN ('unpaid','paid','legacy_incomplete')),
  paid_on DATE,
  payment_method TEXT,
  paid_from TEXT,
  cash_movement_id UUID,
  idempotency_key TEXT NOT NULL,
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  CONSTRAINT uq_inventory_purchases_stock_movement UNIQUE (stock_movement_id),
  CONSTRAINT uq_inventory_purchases_cash_movement UNIQUE (cash_movement_id),
  CONSTRAINT uq_inventory_purchases_idempotency UNIQUE (gym_id,idempotency_key),
  CONSTRAINT chk_inventory_purchases_method CHECK (
    payment_method IS NULL OR payment_method IN ('cash','transfer','card')
  ),
  CONSTRAINT chk_inventory_purchases_paid_from CHECK (
    paid_from IS NULL OR paid_from IN ('cash_drawer','gym_fund','external')
  ),
  CONSTRAINT chk_inventory_purchases_paid_shape CHECK (
    (status = 'paid'
      AND unit_cost > 0 AND total_amount > 0
      AND total_amount = ROUND(unit_cost * quantity, 2)
      AND paid_on IS NOT NULL AND payment_method IS NOT NULL AND paid_from IS NOT NULL)
    OR (status = 'unpaid'
      AND paid_on IS NULL AND payment_method IS NULL AND paid_from IS NULL
      AND cash_movement_id IS NULL
      AND (unit_cost IS NULL OR unit_cost > 0)
      AND (total_amount IS NULL OR total_amount > 0))
    OR status = 'legacy_incomplete'
  ),
  CONSTRAINT chk_inventory_purchase_drawer_method CHECK (
    paid_from <> 'cash_drawer' OR payment_method = 'cash'
  ),
  CONSTRAINT chk_inventory_purchase_cash_link CHECK (
    (status = 'paid' AND paid_from = 'cash_drawer' AND cash_movement_id IS NOT NULL)
    OR (paid_from IS DISTINCT FROM 'cash_drawer' AND cash_movement_id IS NULL)
    OR status = 'legacy_incomplete'
  )
);

ALTER TABLE cash_movements
  DROP CONSTRAINT IF EXISTS cash_movements_classification_status_check;
ALTER TABLE cash_movements
  DROP CONSTRAINT IF EXISTS chk_cash_movements_classification_status;
ALTER TABLE cash_movements ADD CONSTRAINT chk_cash_movements_classification_status
  CHECK (classification_status IN ('unclassified','expense','inventory_purchase','non_operating'));
ALTER TABLE inventory_purchases ADD CONSTRAINT fk_inventory_purchase_cash_movement
  FOREIGN KEY (cash_movement_id) REFERENCES cash_movements(id) ON DELETE RESTRICT;

-- Refund is an aggregate over the root obligation, not a single parent row.
CREATE TABLE refunds (
  id UUID PRIMARY KEY,
  gym_id UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  deleted_at TIMESTAMPTZ,
  root_payment_id UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
  refund_payment_id UUID REFERENCES payments(id) ON DELETE RESTRICT,
  sale_id UUID REFERENCES sales(id) ON DELETE RESTRICT,
  amount NUMERIC(12,2) NOT NULL CHECK (amount >= 0),
  method TEXT CHECK (method IN ('cash','transfer','card')),
  refunded_on DATE NOT NULL,
  reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 200),
  kind TEXT NOT NULL DEFAULT 'revenue_refund'
    CHECK (kind IN ('revenue_refund','overcollection_settlement')),
  balance_cancelled NUMERIC(12,2) NOT NULL DEFAULT 0 CHECK (balance_cancelled >= 0),
  legacy_incomplete BOOLEAN NOT NULL DEFAULT FALSE,
  correction_id UUID,
  idempotency_key TEXT NOT NULL,
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  CONSTRAINT chk_refunds_effect CHECK (
    (amount > 0 AND method IS NOT NULL) OR
    (amount = 0 AND balance_cancelled > 0 AND method IS NULL)
  ),
  CONSTRAINT uq_refunds_payment UNIQUE (refund_payment_id),
  CONSTRAINT uq_refunds_idempotency UNIQUE (gym_id,idempotency_key)
);

CREATE TABLE refund_items (
  id UUID PRIMARY KEY,
  gym_id UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  deleted_at TIMESTAMPTZ,
  refund_id UUID NOT NULL REFERENCES refunds(id) ON DELETE RESTRICT,
  sale_item_id UUID NOT NULL REFERENCES sale_items(id) ON DELETE RESTRICT,
  quantity INTEGER NOT NULL CHECK (quantity > 0),
  amount NUMERIC(12,2) NOT NULL CHECK (amount > 0),
  disposition TEXT NOT NULL CHECK (disposition IN ('returned_to_stock','damaged','not_returned')),
  CONSTRAINT uq_refund_items_line UNIQUE (refund_id,sale_item_id)
);

-- Corrections are exceptional mutations whose complete before/after state is
-- retained as integer-cent JSON snapshots. A physical refund, when needed,
-- remains a separate aggregate referenced by refund_id.
CREATE TABLE sale_corrections (
  id UUID PRIMARY KEY,
  gym_id UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  deleted_at TIMESTAMPTZ,
  sale_id UUID NOT NULL REFERENCES sales(id) ON DELETE RESTRICT,
  expected_sale_version INTEGER NOT NULL CHECK (expected_sale_version >= 0),
  reason TEXT NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 200),
  money_resolution TEXT NOT NULL CHECK (money_resolution IN ('record_only','refund_excess','refund_pending')),
  monetary_delta NUMERIC(12,2) NOT NULL,
  before_snapshot JSONB NOT NULL,
  after_snapshot JSONB NOT NULL,
  refund_id UUID REFERENCES refunds(id) ON DELETE RESTRICT,
  idempotency_key TEXT NOT NULL,
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  CONSTRAINT uq_sale_corrections_idempotency UNIQUE (gym_id,idempotency_key)
);
ALTER TABLE refunds ADD CONSTRAINT fk_refunds_correction
  FOREIGN KEY (correction_id) REFERENCES sale_corrections(id) ON DELETE RESTRICT;

-- Historical refund rows already contain reliable money/date/method data.
-- Product disposition is unknown and is therefore explicitly incomplete.
INSERT INTO refunds (
  id,gym_id,version,created_at,updated_at,deleted_at,
  root_payment_id,refund_payment_id,sale_id,amount,method,refunded_on,
  reason,balance_cancelled,legacy_incomplete,idempotency_key,created_by
)
SELECT p.id,p.gym_id,p.version,p.created_at,p.updated_at,p.deleted_at,
       p.parent_payment_id,p.id,s.id,ABS(p.amount),p.payment_method,p.payment_date,
       COALESCE(NULLIF(BTRIM(p.notes),''),'Devolución legacy'),0,TRUE,
       'legacy:' || p.id::text,p.operator_id
FROM payments p
LEFT JOIN sales s ON s.payment_id=p.parent_payment_id AND s.deleted_at IS NULL
WHERE p.concept='refund' AND p.parent_payment_id IS NOT NULL
ON CONFLICT (refund_payment_id) DO NOTHING;

-- Existing data remains loadable, while every new write must respect signs,
-- references and cent-safe costs. NOT VALID avoids pretending legacy costs are
-- known/clean; it still protects new rows immediately in PostgreSQL.
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_method;
ALTER TABLE payments ADD CONSTRAINT chk_payments_method CHECK (
  payment_method IN ('cash','transfer','card')
);
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_sign;
ALTER TABLE payments ADD CONSTRAINT chk_payments_sign CHECK (
  (concept = 'refund' AND amount < 0) OR (concept <> 'refund' AND amount > 0)
) NOT VALID;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_parent_shape;
ALTER TABLE payments ADD CONSTRAINT chk_payments_parent_shape CHECK (
  (concept IN ('refund','balance_settlement') AND parent_payment_id IS NOT NULL)
  OR (concept NOT IN ('refund','balance_settlement') AND parent_payment_id IS NULL)
) NOT VALID;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_balance_nonnegative;
ALTER TABLE payments ADD CONSTRAINT chk_payments_balance_nonnegative CHECK (balance_pending >= 0) NOT VALID;

ALTER TABLE stock_movements DROP CONSTRAINT IF EXISTS chk_stock_movements_positive_cost;
ALTER TABLE stock_movements ADD CONSTRAINT chk_stock_movements_positive_cost
  CHECK (cost IS NULL OR cost > 0) NOT VALID;

CREATE INDEX idx_inventory_purchases_gym_paid
  ON inventory_purchases(gym_id,paid_on,id) WHERE deleted_at IS NULL AND status='paid';
CREATE INDEX idx_inventory_purchases_product
  ON inventory_purchases(product_id,created_at,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_inventory_purchases_sync
  ON inventory_purchases(gym_id,updated_at);
CREATE INDEX idx_refunds_root ON refunds(root_payment_id,refunded_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refunds_correction_kind ON refunds(correction_id,kind,refunded_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refunds_gym_date ON refunds(gym_id,refunded_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refunds_sync ON refunds(gym_id,updated_at);
CREATE INDEX idx_refund_items_sale_item ON refund_items(sale_item_id,created_at,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refund_items_sync ON refund_items(gym_id,updated_at);
CREATE INDEX idx_sale_corrections_sale ON sale_corrections(sale_id,created_at,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_sale_corrections_sync ON sale_corrections(gym_id,updated_at);

INSERT INTO _migrations(version,name) VALUES(39,'039_financial_integrity')
ON CONFLICT(version) DO NOTHING;
COMMIT;
