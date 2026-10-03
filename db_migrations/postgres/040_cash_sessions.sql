-- ADR-011: turn legacy one-cut-per-day snapshots into physical drawer
-- sessions while retaining the table/entity name for wire compatibility.
BEGIN;

-- Stable physical-drawer catalog. `id=gym_id` is the deterministic main
-- drawer, so legacy clients need no bootstrap request; custom drawers derive
-- their UUID from (gym,idempotency_key) in the application layer.
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
  idempotency_key TEXT CHECK (idempotency_key IS NULL OR char_length(trim(idempotency_key)) BETWEEN 1 AND 120),
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
CREATE INDEX IF NOT EXISTS idx_cash_drawers_sync ON cash_drawers(gym_id,updated_at);

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

ALTER TABLE cash_close_events
  ADD COLUMN IF NOT EXISTS drawer_id UUID,
  ADD COLUMN IF NOT EXISTS drawer_code TEXT,
  ADD COLUMN IF NOT EXISTS operational_date DATE,
  ADD COLUMN IF NOT EXISTS sequence INTEGER,
  ADD COLUMN IF NOT EXISTS status TEXT,
  ADD COLUMN IF NOT EXISTS opening_cash NUMERIC(12,2),
  ADD COLUMN IF NOT EXISTS opening_cash_known BOOLEAN,
  ADD COLUMN IF NOT EXISTS activity_cash NUMERIC(12,2),
  ADD COLUMN IF NOT EXISTS cash_left NUMERIC(12,2),
  ADD COLUMN IF NOT EXISTS withdrawn_cash NUMERIC(12,2),
  ADD COLUMN IF NOT EXISTS withdrawal_destination TEXT,
  ADD COLUMN IF NOT EXISTS correction_reason TEXT,
  ADD COLUMN IF NOT EXISTS adjusted_after_withdrawal BOOLEAN,
  ADD COLUMN IF NOT EXISTS integrity_note TEXT,
  ADD COLUMN IF NOT EXISTS opened_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS opened_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS reconciled_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS reconciled_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  ADD COLUMN IF NOT EXISTS stale_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS withdrawn_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS withdrawn_by UUID REFERENCES users(id) ON DELETE RESTRICT;

-- Physical writers default to the main drawer when this nullable attribution
-- is absent. The nullable shape keeps old clients forward-compatible while
-- allowing multiple drawers/turns through the same ledger.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS cash_drawer_id UUID;
ALTER TABLE cash_movements ADD COLUMN IF NOT EXISTS cash_drawer_id UUID;
UPDATE payments SET cash_drawer_id=gym_id
 WHERE cash_drawer_id IS NULL AND payment_method='cash';
UPDATE cash_movements SET cash_drawer_id=gym_id WHERE cash_drawer_id IS NULL;
ALTER TABLE payments ADD CONSTRAINT fk_payments_cash_drawer
  FOREIGN KEY(gym_id,cash_drawer_id) REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT;
ALTER TABLE cash_movements ADD CONSTRAINT fk_cash_movements_drawer
  FOREIGN KEY(gym_id,cash_drawer_id) REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT;
CREATE INDEX IF NOT EXISTS idx_payments_cash_drawer_session
  ON payments(gym_id,cash_drawer_id,payment_date,created_at) WHERE deleted_at IS NULL AND payment_method='cash';
CREATE INDEX IF NOT EXISTS idx_cash_movements_drawer_session
  ON cash_movements(gym_id,cash_drawer_id,movement_on,created_at) WHERE deleted_at IS NULL;

-- Legacy cuts never recorded their opening float or withdrawal. Keep those
-- unknown instead of inventing a delivered amount; opening_cash_known keeps
-- range coverage from claiming that the old reconciliation is complete.
UPDATE cash_close_events
SET drawer_id = COALESCE(drawer_id, gym_id),
    drawer_code = COALESCE(NULLIF(drawer_code, ''), 'main'),
    operational_date = COALESCE(operational_date, close_date),
    sequence = COALESCE(sequence, 1),
    status = COALESCE(status, CASE WHEN counted_cash IS NULL THEN 'closed_unverified' ELSE 'reconciled' END),
    opening_cash = COALESCE(opening_cash, 0),
    opening_cash_known = COALESCE(opening_cash_known, FALSE),
    activity_cash = COALESCE(activity_cash, calculated_cash),
    adjusted_after_withdrawal = COALESCE(adjusted_after_withdrawal, FALSE),
    -- A legacy cut represented the whole operational day. Starting it at the
    -- old row's creation time would silently omit every payment captured
    -- earlier that day when the session is recalculated.
    opened_at = COALESCE(opened_at,
      COALESCE(operational_date, close_date)::timestamp AT TIME ZONE 'UTC'),
    opened_by = COALESCE(opened_by, closed_by),
    closed_at = COALESCE(closed_at, created_at),
    reconciled_at = CASE WHEN counted_cash IS NOT NULL THEN COALESCE(reconciled_at, created_at) ELSE reconciled_at END,
    reconciled_by = CASE WHEN counted_cash IS NOT NULL THEN COALESCE(reconciled_by, closed_by) ELSE reconciled_by END;

ALTER TABLE cash_close_events
  ALTER COLUMN drawer_id SET NOT NULL,
  ALTER COLUMN drawer_code SET NOT NULL,
  ALTER COLUMN drawer_code SET DEFAULT 'main',
  ALTER COLUMN operational_date SET NOT NULL,
  ALTER COLUMN sequence SET NOT NULL,
  ALTER COLUMN sequence SET DEFAULT 1,
  ALTER COLUMN status SET NOT NULL,
  ALTER COLUMN status SET DEFAULT 'open',
  ALTER COLUMN opening_cash SET NOT NULL,
  ALTER COLUMN opening_cash SET DEFAULT 0,
  ALTER COLUMN opening_cash_known SET NOT NULL,
  ALTER COLUMN opening_cash_known SET DEFAULT TRUE,
  ALTER COLUMN activity_cash SET NOT NULL,
  ALTER COLUMN activity_cash SET DEFAULT 0,
  ALTER COLUMN adjusted_after_withdrawal SET NOT NULL,
  ALTER COLUMN adjusted_after_withdrawal SET DEFAULT FALSE,
  ALTER COLUMN opened_at SET NOT NULL,
  ALTER COLUMN opened_by SET NOT NULL;

ALTER TABLE cash_close_events ADD CONSTRAINT fk_cash_sessions_drawer
  FOREIGN KEY(gym_id,drawer_id) REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT;

DROP INDEX IF EXISTS uq_cash_close_events_gym_date;
CREATE UNIQUE INDEX uq_cash_sessions_natural
  ON cash_close_events(gym_id, drawer_id, operational_date, sequence)
  WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_cash_sessions_gym_day
  ON cash_close_events(gym_id, operational_date, drawer_id, sequence DESC)
  WHERE deleted_at IS NULL;

ALTER TABLE cash_close_events DROP COLUMN IF EXISTS discrepancy;
ALTER TABLE cash_close_events ADD COLUMN discrepancy NUMERIC(12,2)
  GENERATED ALWAYS AS (
    CASE WHEN status IN ('reconciled','withdrawn') AND counted_cash IS NOT NULL
              AND adjusted_after_withdrawal=FALSE
      THEN counted_cash - calculated_cash ELSE NULL END
  ) STORED;

ALTER TABLE cash_close_events
  ADD CONSTRAINT chk_cash_sessions_status CHECK
    (status IN ('open','closed_unverified','reconciled','stale','withdrawn')),
  ADD CONSTRAINT chk_cash_sessions_sequence CHECK (sequence > 0),
  ADD CONSTRAINT chk_cash_sessions_drawer_code CHECK (char_length(trim(drawer_code)) BETWEEN 1 AND 40),
  ADD CONSTRAINT chk_cash_sessions_opening CHECK (opening_cash >= 0),
  ADD CONSTRAINT chk_cash_sessions_count CHECK (counted_cash IS NULL OR counted_cash >= 0),
  ADD CONSTRAINT chk_cash_sessions_left CHECK (cash_left IS NULL OR cash_left >= 0),
  ADD CONSTRAINT chk_cash_sessions_withdrawn CHECK (withdrawn_cash IS NULL OR withdrawn_cash >= 0),
  ADD CONSTRAINT chk_cash_sessions_withdrawal_math CHECK
    (withdrawn_cash IS NULL OR (counted_cash IS NOT NULL AND cash_left IS NOT NULL
      AND withdrawn_cash = counted_cash - cash_left)),
  ADD CONSTRAINT chk_cash_sessions_destination CHECK
    (withdrawal_destination IS NULL OR withdrawal_destination = 'gym_fund'),
  ADD CONSTRAINT chk_cash_sessions_adjustment_shape CHECK
    (adjusted_after_withdrawal=FALSE OR
      (status='withdrawn' AND integrity_note IS NOT NULL AND char_length(trim(integrity_note)) > 0)),
  ADD CONSTRAINT chk_cash_sessions_lifecycle CHECK (
    (status = 'open' AND closed_at IS NULL AND counted_cash IS NULL AND withdrawn_at IS NULL)
    OR (status = 'closed_unverified' AND closed_at IS NOT NULL AND counted_cash IS NULL AND withdrawn_at IS NULL)
    OR (status = 'reconciled' AND closed_at IS NOT NULL AND counted_cash IS NOT NULL
        AND reconciled_at IS NOT NULL AND reconciled_by IS NOT NULL AND withdrawn_at IS NULL)
    OR (status = 'stale' AND closed_at IS NOT NULL AND stale_at IS NOT NULL
        AND correction_reason IS NOT NULL AND char_length(trim(correction_reason)) > 0
        AND withdrawn_at IS NULL)
    OR (status = 'withdrawn' AND closed_at IS NOT NULL AND counted_cash IS NOT NULL
        AND reconciled_at IS NOT NULL AND reconciled_by IS NOT NULL
        AND cash_left IS NOT NULL AND withdrawn_cash IS NOT NULL
        AND withdrawal_destination='gym_fund' AND withdrawn_at IS NOT NULL AND withdrawn_by IS NOT NULL)
  );

-- A withdrawal is a transfer between locations, never an Expense. The row is
-- one-to-one with the session and syncs independently for an auditable event.
CREATE TABLE IF NOT EXISTS cash_transfers (
  id UUID PRIMARY KEY,
  gym_id UUID NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  deleted_at TIMESTAMPTZ,
  session_id UUID NOT NULL REFERENCES cash_close_events(id) ON UPDATE CASCADE ON DELETE RESTRICT,
  drawer_id UUID NOT NULL,
  destination TEXT NOT NULL CHECK (destination = 'gym_fund'),
  amount NUMERIC(12,2) NOT NULL CHECK (amount >= 0),
  transferred_at TIMESTAMPTZ NOT NULL,
  transferred_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  CONSTRAINT fk_cash_transfers_drawer FOREIGN KEY(gym_id,drawer_id)
    REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_transfers_session
  ON cash_transfers(session_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_cash_transfers_gym_time
  ON cash_transfers(gym_id, transferred_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_cash_transfers_sync
  ON cash_transfers(gym_id, updated_at);

-- Refresh existing journal payloads. Old sidecars ignore unknown fields; new
-- sidecars need them to avoid replacing a migrated session with defaults.
UPDATE sync_entities se
SET payload = se.payload || jsonb_build_object(
  'drawer_id', e.drawer_id,
  'drawer_code', e.drawer_code,
  'operational_date', e.operational_date,
  'sequence', e.sequence,
  'status', e.status,
  'opening_cash', e.opening_cash,
  'opening_cash_known', e.opening_cash_known,
  'activity_cash', e.activity_cash,
  'cash_left', e.cash_left,
  'withdrawn_cash', e.withdrawn_cash,
  'withdrawal_destination', e.withdrawal_destination,
  'correction_reason', e.correction_reason,
  'adjusted_after_withdrawal', e.adjusted_after_withdrawal,
  'integrity_note', e.integrity_note,
  'opened_at', (EXTRACT(EPOCH FROM e.opened_at) * 1000)::bigint,
  'opened_by', e.opened_by,
  'closed_at', CASE WHEN e.closed_at IS NULL THEN NULL ELSE (EXTRACT(EPOCH FROM e.closed_at) * 1000)::bigint END,
  'reconciled_at', CASE WHEN e.reconciled_at IS NULL THEN NULL ELSE (EXTRACT(EPOCH FROM e.reconciled_at) * 1000)::bigint END,
  'reconciled_by', e.reconciled_by,
  'stale_at', CASE WHEN e.stale_at IS NULL THEN NULL ELSE (EXTRACT(EPOCH FROM e.stale_at) * 1000)::bigint END,
  'withdrawn_at', CASE WHEN e.withdrawn_at IS NULL THEN NULL ELSE (EXTRACT(EPOCH FROM e.withdrawn_at) * 1000)::bigint END,
  'withdrawn_by', e.withdrawn_by
)
FROM cash_close_events e
WHERE se.entity_type = 'cash_close_events'
  AND se.gym_id = e.gym_id AND se.entity_id = e.id;

UPDATE sync_entities se
SET payload = se.payload || jsonb_build_object('cash_drawer_id', p.cash_drawer_id)
FROM payments p
WHERE se.entity_type='payments' AND se.gym_id=p.gym_id AND se.entity_id=p.id;
UPDATE sync_entities se
SET payload = se.payload || jsonb_build_object('cash_drawer_id', m.cash_drawer_id)
FROM cash_movements m
WHERE se.entity_type='cash_movements' AND se.gym_id=m.gym_id AND se.entity_id=m.id;

INSERT INTO _migrations(version, name) VALUES(40, '040_cash_sessions')
ON CONFLICT(version) DO NOTHING;

COMMIT;
