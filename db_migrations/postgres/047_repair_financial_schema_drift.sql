-- Repair installations that recorded migrations 039/040 before their final
-- financial-integrity and cash-session shape was frozen. Those databases
-- legitimately contain migration markers 39/40, so editing the historical
-- files cannot repair them: the missing delta must have its own version.
BEGIN;

-- Canonical payment recognition and correction metadata (final PG 039 shape).
ALTER TABLE sales
  ADD COLUMN IF NOT EXISTS correction_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sales DROP CONSTRAINT IF EXISTS chk_sales_correction_version;
ALTER TABLE sales ADD CONSTRAINT chk_sales_correction_version
  CHECK (correction_version >= 0);

ALTER TABLE payments ADD COLUMN IF NOT EXISTS recognized_amount NUMERIC(12,2);
ALTER TABLE payments ADD COLUMN IF NOT EXISTS membership_id UUID;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS idempotency_fingerprint TEXT;
ALTER TABLE payments ADD COLUMN IF NOT EXISTS idempotency_result JSONB;
UPDATE payments
SET recognized_amount=CASE WHEN concept='refund' THEN 0 ELSE amount END
WHERE recognized_amount IS NULL;
ALTER TABLE payments ALTER COLUMN recognized_amount SET DEFAULT 0;
ALTER TABLE payments ALTER COLUMN recognized_amount SET NOT NULL;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_membership_id_fkey;
ALTER TABLE payments ADD CONSTRAINT payments_membership_id_fkey
  FOREIGN KEY(membership_id) REFERENCES memberships(id) ON DELETE RESTRICT;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_recognized_amount;
ALTER TABLE payments ADD CONSTRAINT chk_payments_recognized_amount
  CHECK (recognized_amount >= 0);
ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_idempotency_shape;
ALTER TABLE payments ADD CONSTRAINT chk_payments_idempotency_shape CHECK (
  (idempotency_key IS NULL AND idempotency_fingerprint IS NULL AND idempotency_result IS NULL)
  OR (char_length(BTRIM(idempotency_key)) BETWEEN 1 AND 120
      AND char_length(BTRIM(idempotency_fingerprint)) > 0)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_payments_idempotency
  ON payments(gym_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;

-- A debt-only refund cancels an obligation without moving physical money.
-- Older PG 039 revisions required a method/payment row and lacked `kind`.
ALTER TABLE refunds ADD COLUMN IF NOT EXISTS kind TEXT;
UPDATE refunds SET kind='revenue_refund' WHERE kind IS NULL;
ALTER TABLE refunds ALTER COLUMN kind SET DEFAULT 'revenue_refund';
ALTER TABLE refunds ALTER COLUMN kind SET NOT NULL;
ALTER TABLE refunds ALTER COLUMN method DROP NOT NULL;
ALTER TABLE refunds ALTER COLUMN refund_payment_id DROP NOT NULL;
ALTER TABLE refunds DROP CONSTRAINT IF EXISTS refunds_kind_check;
ALTER TABLE refunds DROP CONSTRAINT IF EXISTS chk_refunds_kind;
ALTER TABLE refunds ADD CONSTRAINT refunds_kind_check
  CHECK (kind IN ('revenue_refund','overcollection_settlement'));
ALTER TABLE refunds DROP CONSTRAINT IF EXISTS chk_refunds_effect;
ALTER TABLE refunds ADD CONSTRAINT chk_refunds_effect CHECK (
  (amount > 0 AND method IS NOT NULL) OR
  (amount = 0 AND balance_cancelled > 0 AND method IS NULL)
);

-- Stable physical drawer catalog (final PG 040 shape).
CREATE TABLE IF NOT EXISTS cash_drawers (
  id UUID PRIMARY KEY,
  gym_id UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1 CHECK (version>0),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  deleted_at TIMESTAMPTZ,
  code TEXT NOT NULL CHECK (char_length(trim(code)) BETWEEN 1 AND 40),
  name TEXT NOT NULL CHECK (char_length(trim(name)) BETWEEN 1 AND 60),
  active BOOLEAN NOT NULL DEFAULT TRUE,
  is_main BOOLEAN NOT NULL DEFAULT FALSE,
  idempotency_key TEXT CHECK (
    idempotency_key IS NULL OR char_length(trim(idempotency_key)) BETWEEN 1 AND 120
  ),
  CHECK ((is_main AND id=gym_id AND code='main' AND active) OR
         (NOT is_main AND id<>gym_id AND code<>'main')),
  UNIQUE(gym_id,id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_name
  ON cash_drawers(gym_id,lower(name)) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_code
  ON cash_drawers(gym_id,lower(code)) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_main
  ON cash_drawers(gym_id) WHERE is_main AND deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_idempotency
  ON cash_drawers(gym_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_cash_drawers_sync
  ON cash_drawers(gym_id,updated_at);

INSERT INTO cash_drawers(id,gym_id,version,created_at,updated_at,code,name,active,is_main)
SELECT id,id,1,created_at,updated_at,'main','Caja principal',TRUE,TRUE FROM gyms
ON CONFLICT(id) DO NOTHING;

CREATE OR REPLACE FUNCTION seed_main_cash_drawer() RETURNS trigger AS $$
BEGIN
  INSERT INTO cash_drawers(id,gym_id,version,created_at,updated_at,code,name,active,is_main)
  VALUES(NEW.id,NEW.id,1,COALESCE(NEW.created_at,NOW()),COALESCE(NEW.updated_at,NOW()),
         'main','Caja principal',TRUE,TRUE)
  ON CONFLICT(id) DO NOTHING;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_gyms_seed_main_cash_drawer ON gyms;
CREATE TRIGGER trg_gyms_seed_main_cash_drawer AFTER INSERT ON gyms
FOR EACH ROW EXECUTE FUNCTION seed_main_cash_drawer();

ALTER TABLE payments ADD COLUMN IF NOT EXISTS cash_drawer_id UUID;
ALTER TABLE cash_movements ADD COLUMN IF NOT EXISTS cash_drawer_id UUID;
UPDATE payments SET cash_drawer_id=gym_id
 WHERE cash_drawer_id IS NULL AND payment_method='cash';
UPDATE cash_movements SET cash_drawer_id=gym_id WHERE cash_drawer_id IS NULL;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS fk_payments_cash_drawer;
ALTER TABLE payments ADD CONSTRAINT fk_payments_cash_drawer
  FOREIGN KEY(gym_id,cash_drawer_id)
  REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT;
ALTER TABLE cash_movements DROP CONSTRAINT IF EXISTS fk_cash_movements_drawer;
ALTER TABLE cash_movements ADD CONSTRAINT fk_cash_movements_drawer
  FOREIGN KEY(gym_id,cash_drawer_id)
  REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT;
CREATE INDEX IF NOT EXISTS idx_payments_cash_drawer_session
  ON payments(gym_id,cash_drawer_id,payment_date,created_at)
  WHERE deleted_at IS NULL AND payment_method='cash';
CREATE INDEX IF NOT EXISTS idx_cash_movements_drawer_session
  ON cash_movements(gym_id,cash_drawer_id,movement_on,created_at)
  WHERE deleted_at IS NULL;

ALTER TABLE cash_close_events
  ADD COLUMN IF NOT EXISTS adjusted_after_withdrawal BOOLEAN,
  ADD COLUMN IF NOT EXISTS integrity_note TEXT;
UPDATE cash_close_events
SET adjusted_after_withdrawal=COALESCE(adjusted_after_withdrawal,FALSE);
ALTER TABLE cash_close_events
  ALTER COLUMN adjusted_after_withdrawal SET DEFAULT FALSE,
  ALTER COLUMN adjusted_after_withdrawal SET NOT NULL;
ALTER TABLE cash_close_events DROP CONSTRAINT IF EXISTS fk_cash_sessions_drawer;
ALTER TABLE cash_close_events ADD CONSTRAINT fk_cash_sessions_drawer
  FOREIGN KEY(gym_id,drawer_id)
  REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT;
ALTER TABLE cash_close_events DROP CONSTRAINT IF EXISTS chk_cash_sessions_adjustment_shape;
ALTER TABLE cash_close_events ADD CONSTRAINT chk_cash_sessions_adjustment_shape CHECK
  (adjusted_after_withdrawal=FALSE OR
    (status='withdrawn' AND integrity_note IS NOT NULL AND char_length(trim(integrity_note))>0));
ALTER TABLE cash_close_events DROP CONSTRAINT IF EXISTS chk_cash_sessions_lifecycle;
ALTER TABLE cash_close_events ADD CONSTRAINT chk_cash_sessions_lifecycle CHECK (
  (status='open' AND closed_at IS NULL AND counted_cash IS NULL AND withdrawn_at IS NULL)
  OR (status='closed_unverified' AND closed_at IS NOT NULL AND counted_cash IS NULL AND withdrawn_at IS NULL)
  OR (status='reconciled' AND closed_at IS NOT NULL AND counted_cash IS NOT NULL
      AND reconciled_at IS NOT NULL AND reconciled_by IS NOT NULL AND withdrawn_at IS NULL)
  OR (status='stale' AND closed_at IS NOT NULL AND stale_at IS NOT NULL
      AND correction_reason IS NOT NULL AND char_length(trim(correction_reason))>0
      AND withdrawn_at IS NULL)
  OR (status='withdrawn' AND closed_at IS NOT NULL AND counted_cash IS NOT NULL
      AND reconciled_at IS NOT NULL AND reconciled_by IS NOT NULL
      AND cash_left IS NOT NULL AND withdrawn_cash IS NOT NULL
      AND withdrawal_destination='gym_fund' AND withdrawn_at IS NOT NULL AND withdrawn_by IS NOT NULL)
);
ALTER TABLE cash_close_events DROP COLUMN IF EXISTS discrepancy;
ALTER TABLE cash_close_events ADD COLUMN discrepancy NUMERIC(12,2)
  GENERATED ALWAYS AS (
    CASE WHEN status IN ('reconciled','withdrawn') AND counted_cash IS NOT NULL
              AND adjusted_after_withdrawal=FALSE
      THEN counted_cash-calculated_cash ELSE NULL END
  ) STORED;

ALTER TABLE cash_transfers DROP CONSTRAINT IF EXISTS fk_cash_transfers_drawer;
ALTER TABLE cash_transfers ADD CONSTRAINT fk_cash_transfers_drawer
  FOREIGN KEY(gym_id,drawer_id)
  REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT;

-- Preserve the relaxed tombstone path from PG 046 while repairing old copies
-- of the constraint that lacked it.
ALTER TABLE cash_movements
  DROP CONSTRAINT IF EXISTS chk_cash_movements_cash_in_non_operating;
ALTER TABLE cash_movements ADD CONSTRAINT chk_cash_movements_cash_in_non_operating
  CHECK (deleted_at IS NOT NULL OR movement_type<>'cash_in' OR
    (classification_status='non_operating' AND expense_id IS NULL)) NOT VALID;

-- Refresh existing journal payloads. No version bump is required: these are
-- compatibility fields derived from the same canonical version, and the
-- sidecar already defaults absent fields when reading historical payloads.
UPDATE sync_entities se
SET payload=se.payload || jsonb_build_object(
  'recognized_amount',p.recognized_amount,
  'membership_id',p.membership_id,
  'idempotency_key',p.idempotency_key,
  'idempotency_fingerprint',p.idempotency_fingerprint,
  'idempotency_result',p.idempotency_result,
  'cash_drawer_id',p.cash_drawer_id
)
FROM payments p
WHERE se.entity_type='payments' AND se.gym_id=p.gym_id AND se.entity_id=p.id;

UPDATE sync_entities se
SET payload=se.payload || jsonb_build_object('correction_version',s.correction_version)
FROM sales s
WHERE se.entity_type='sales' AND se.gym_id=s.gym_id AND se.entity_id=s.id;

UPDATE sync_entities se
SET payload=se.payload || jsonb_build_object('kind',r.kind)
FROM refunds r
WHERE se.entity_type='refunds' AND se.gym_id=r.gym_id AND se.entity_id=r.id;

UPDATE sync_entities se
SET payload=se.payload || jsonb_build_object('cash_drawer_id',m.cash_drawer_id)
FROM cash_movements m
WHERE se.entity_type='cash_movements' AND se.gym_id=m.gym_id AND se.entity_id=m.id;

UPDATE sync_entities se
SET payload=se.payload || jsonb_build_object(
  'adjusted_after_withdrawal',e.adjusted_after_withdrawal,
  'integrity_note',e.integrity_note
)
FROM cash_close_events e
WHERE se.entity_type='cash_close_events'
  AND se.gym_id=e.gym_id AND se.entity_id=e.id;

INSERT INTO _migrations(version,name)
VALUES(47,'047_repair_financial_schema_drift')
ON CONFLICT(version) DO NOTHING;

COMMIT;
