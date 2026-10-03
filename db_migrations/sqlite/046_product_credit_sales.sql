-- Product sales can be entirely on credit. Other payment signs remain unchanged.
PRAGMA foreign_keys = OFF;
BEGIN;
CREATE TABLE payments_credit_new (
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
  cash_drawer_id TEXT,
  cash_destination TEXT NOT NULL DEFAULT 'cash_drawer' CHECK(cash_destination IN ('cash_drawer','gym_fund')),
  CHECK((concept='refund' AND amount<0) OR (concept='product' AND amount>=0) OR (concept NOT IN ('refund','product') AND amount>0)),
  CHECK((idempotency_key IS NULL AND idempotency_fingerprint IS NULL AND idempotency_result IS NULL)
     OR (length(trim(idempotency_key)) BETWEEN 1 AND 120 AND length(trim(idempotency_fingerprint))>0)),
  CHECK((concept IN ('refund','balance_settlement') AND parent_payment_id IS NOT NULL)
     OR (concept NOT IN ('refund','balance_settlement') AND parent_payment_id IS NULL))
);
INSERT INTO payments_credit_new(id,gym_id,version,created_at,updated_at,deleted_at,synced_at,folio,member_id,membership_id,idempotency_key,idempotency_fingerprint,idempotency_result,amount,recognized_amount,payment_method,concept,parent_payment_id,discount_amount,discount_reason,balance_pending,payment_date,notes,operator_id,breakdown,cash_drawer_id,cash_destination) SELECT id,gym_id,version,created_at,updated_at,deleted_at,synced_at,folio,member_id,membership_id,idempotency_key,idempotency_fingerprint,idempotency_result,amount,recognized_amount,payment_method,concept,parent_payment_id,discount_amount,discount_reason,balance_pending,payment_date,notes,operator_id,breakdown,cash_drawer_id,cash_destination FROM payments;
DROP TABLE payments;
ALTER TABLE payments_credit_new RENAME TO payments;
CREATE UNIQUE INDEX uq_payments_gym_folio ON payments(gym_id,folio) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX uq_payments_idempotency ON payments(gym_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX idx_payments_member ON payments(member_id,payment_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_payments_gym_date ON payments(gym_id,payment_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_payments_sync ON payments(gym_id,updated_at);
CREATE INDEX idx_payments_concept ON payments(gym_id,concept,payment_date DESC) WHERE deleted_at IS NULL;

CREATE TRIGGER IF NOT EXISTS trg_payments_cash_drawer_insert
BEFORE INSERT ON payments WHEN NEW.cash_drawer_id IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM cash_drawers d WHERE d.gym_id=NEW.gym_id AND d.id=NEW.cash_drawer_id AND d.deleted_at IS NULL)
BEGIN SELECT RAISE(ABORT,'cash drawer does not exist'); END;
CREATE TRIGGER IF NOT EXISTS trg_payments_cash_drawer_update
BEFORE UPDATE OF cash_drawer_id,gym_id ON payments WHEN NEW.cash_drawer_id IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM cash_drawers d WHERE d.gym_id=NEW.gym_id AND d.id=NEW.cash_drawer_id AND d.deleted_at IS NULL)
BEGIN SELECT RAISE(ABORT,'cash drawer does not exist'); END;
CREATE INDEX idx_payments_cash_drawer_session ON payments(gym_id,cash_drawer_id,payment_date,created_at) WHERE deleted_at IS NULL AND payment_method='cash';
INSERT INTO _migrations(version,name,applied_at) VALUES(46,'046_product_credit_sales',CAST(strftime('%s','now') AS INTEGER)*1000);
COMMIT;
PRAGMA foreign_keys = ON;
