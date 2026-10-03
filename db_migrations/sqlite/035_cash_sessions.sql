-- SQLite mirror of PG 040. Money remains integer cents and timestamps epoch-ms.
BEGIN;

CREATE TABLE IF NOT EXISTS cash_drawers (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1 CHECK (version>0),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  code TEXT NOT NULL CHECK (length(trim(code)) BETWEEN 1 AND 40),
  name TEXT NOT NULL CHECK (length(trim(name)) BETWEEN 1 AND 60),
  active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
  is_main INTEGER NOT NULL DEFAULT 0 CHECK (is_main IN (0,1)),
  idempotency_key TEXT CHECK (idempotency_key IS NULL OR length(trim(idempotency_key)) BETWEEN 1 AND 120),
  CHECK ((is_main=1 AND id=gym_id AND code='main' AND active=1) OR
         (is_main=0 AND id<>gym_id AND code<>'main')),
  UNIQUE(gym_id,id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_name
  ON cash_drawers(gym_id,lower(name)) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_code
  ON cash_drawers(gym_id,lower(code)) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_main
  ON cash_drawers(gym_id) WHERE is_main=1 AND deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_idempotency
  ON cash_drawers(gym_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_cash_drawers_sync ON cash_drawers(gym_id,updated_at);

INSERT OR IGNORE INTO cash_drawers(id,gym_id,version,created_at,updated_at,code,name,active,is_main)
SELECT id,id,1,created_at,updated_at,'main','Caja principal',1,1 FROM gyms;
CREATE TRIGGER IF NOT EXISTS trg_gyms_seed_main_cash_drawer
AFTER INSERT ON gyms BEGIN
  INSERT OR IGNORE INTO cash_drawers(id,gym_id,version,created_at,updated_at,code,name,active,is_main)
  VALUES(NEW.id,NEW.id,1,NEW.created_at,NEW.updated_at,'main','Caja principal',1,1);
END;

ALTER TABLE cash_close_events ADD COLUMN drawer_id TEXT;
ALTER TABLE cash_close_events ADD COLUMN drawer_code TEXT;
ALTER TABLE cash_close_events ADD COLUMN operational_date TEXT;
ALTER TABLE cash_close_events ADD COLUMN sequence INTEGER;
ALTER TABLE cash_close_events ADD COLUMN status TEXT;
ALTER TABLE cash_close_events ADD COLUMN opening_cash INTEGER;
ALTER TABLE cash_close_events ADD COLUMN opening_cash_known INTEGER;
ALTER TABLE cash_close_events ADD COLUMN activity_cash INTEGER;
ALTER TABLE cash_close_events ADD COLUMN cash_left INTEGER;
ALTER TABLE cash_close_events ADD COLUMN withdrawn_cash INTEGER;
ALTER TABLE cash_close_events ADD COLUMN withdrawal_destination TEXT;
ALTER TABLE cash_close_events ADD COLUMN correction_reason TEXT;
ALTER TABLE cash_close_events ADD COLUMN adjusted_after_withdrawal INTEGER;
ALTER TABLE cash_close_events ADD COLUMN integrity_note TEXT;
ALTER TABLE cash_close_events ADD COLUMN opened_at INTEGER;
ALTER TABLE cash_close_events ADD COLUMN opened_by TEXT;
ALTER TABLE cash_close_events ADD COLUMN closed_at INTEGER;
ALTER TABLE cash_close_events ADD COLUMN reconciled_at INTEGER;
ALTER TABLE cash_close_events ADD COLUMN reconciled_by TEXT;
ALTER TABLE cash_close_events ADD COLUMN stale_at INTEGER;
ALTER TABLE cash_close_events ADD COLUMN withdrawn_at INTEGER;
ALTER TABLE cash_close_events ADD COLUMN withdrawn_by TEXT;
ALTER TABLE payments ADD COLUMN cash_drawer_id TEXT;
ALTER TABLE cash_movements ADD COLUMN cash_drawer_id TEXT;
UPDATE payments SET cash_drawer_id=gym_id
 WHERE cash_drawer_id IS NULL AND payment_method='cash';
UPDATE cash_movements SET cash_drawer_id=gym_id WHERE cash_drawer_id IS NULL;
CREATE TRIGGER IF NOT EXISTS trg_payments_cash_drawer_insert
BEFORE INSERT ON payments WHEN NEW.cash_drawer_id IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM cash_drawers d WHERE d.gym_id=NEW.gym_id AND d.id=NEW.cash_drawer_id AND d.deleted_at IS NULL)
BEGIN SELECT RAISE(ABORT,'cash drawer does not exist'); END;
CREATE TRIGGER IF NOT EXISTS trg_payments_cash_drawer_update
BEFORE UPDATE OF cash_drawer_id,gym_id ON payments WHEN NEW.cash_drawer_id IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM cash_drawers d WHERE d.gym_id=NEW.gym_id AND d.id=NEW.cash_drawer_id AND d.deleted_at IS NULL)
BEGIN SELECT RAISE(ABORT,'cash drawer does not exist'); END;
CREATE TRIGGER IF NOT EXISTS trg_cash_movements_drawer_insert
BEFORE INSERT ON cash_movements WHEN NEW.cash_drawer_id IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM cash_drawers d WHERE d.gym_id=NEW.gym_id AND d.id=NEW.cash_drawer_id AND d.deleted_at IS NULL)
BEGIN SELECT RAISE(ABORT,'cash drawer does not exist'); END;
CREATE TRIGGER IF NOT EXISTS trg_cash_movements_drawer_update
BEFORE UPDATE OF cash_drawer_id,gym_id ON cash_movements WHEN NEW.cash_drawer_id IS NOT NULL AND NOT EXISTS(
  SELECT 1 FROM cash_drawers d WHERE d.gym_id=NEW.gym_id AND d.id=NEW.cash_drawer_id AND d.deleted_at IS NULL)
BEGIN SELECT RAISE(ABORT,'cash drawer does not exist'); END;
CREATE INDEX IF NOT EXISTS idx_payments_cash_drawer_session
  ON payments(gym_id,cash_drawer_id,payment_date,created_at) WHERE deleted_at IS NULL AND payment_method='cash';
CREATE INDEX IF NOT EXISTS idx_cash_movements_drawer_session
  ON cash_movements(gym_id,cash_drawer_id,movement_on,created_at) WHERE deleted_at IS NULL;

UPDATE cash_close_events
SET drawer_id = COALESCE(drawer_id, gym_id),
    drawer_code = COALESCE(NULLIF(drawer_code, ''), 'main'),
    operational_date = COALESCE(operational_date, close_date),
    sequence = COALESCE(sequence, 1),
    status = COALESCE(status, CASE WHEN counted_cash IS NULL THEN 'closed_unverified' ELSE 'reconciled' END),
    opening_cash = COALESCE(opening_cash, 0),
    opening_cash_known = COALESCE(opening_cash_known, 0),
    activity_cash = COALESCE(activity_cash, calculated_cash),
    adjusted_after_withdrawal = COALESCE(adjusted_after_withdrawal, 0),
    -- Legacy cuts covered their complete date, not only the instant at which
    -- the old immutable snapshot happened to be inserted.
    opened_at = COALESCE(opened_at,
      CAST(strftime('%s', COALESCE(operational_date, close_date) || ' 00:00:00') AS INTEGER) * 1000),
    opened_by = COALESCE(opened_by, closed_by),
    closed_at = COALESCE(closed_at, created_at),
    reconciled_at = CASE WHEN counted_cash IS NOT NULL THEN COALESCE(reconciled_at, created_at) ELSE reconciled_at END,
    reconciled_by = CASE WHEN counted_cash IS NOT NULL THEN COALESCE(reconciled_by, closed_by) ELSE reconciled_by END;

DROP INDEX IF EXISTS uq_cash_close_events_gym_date;
-- SQLite cannot retrofit NOT NULL/CHECK constraints or replace a STORED
-- generated column. Rebuild once so its write barrier and nullable
-- discrepancy have the same meaning as PostgreSQL, including direct sync
-- projection and ad-hoc SQL writers.
ALTER TABLE cash_close_events RENAME TO cash_close_events_legacy_035;
CREATE TABLE cash_close_events (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  close_date TEXT NOT NULL,
  calculated_cash INTEGER NOT NULL,
  counted_cash INTEGER CHECK (counted_cash IS NULL OR counted_cash>=0),
  discrepancy INTEGER GENERATED ALWAYS AS (
    CASE WHEN status IN ('reconciled','withdrawn') AND counted_cash IS NOT NULL
              AND adjusted_after_withdrawal=0
      THEN counted_cash-calculated_cash ELSE NULL END
  ) STORED,
  discrepancy_reason TEXT,
  closed_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  drawer_id TEXT NOT NULL,
  drawer_code TEXT NOT NULL DEFAULT 'main' CHECK (length(trim(drawer_code)) BETWEEN 1 AND 40),
  operational_date TEXT NOT NULL,
  sequence INTEGER NOT NULL DEFAULT 1 CHECK (sequence>0),
  status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed_unverified','reconciled','stale','withdrawn')),
  opening_cash INTEGER NOT NULL DEFAULT 0 CHECK (opening_cash>=0),
  opening_cash_known INTEGER NOT NULL DEFAULT 1 CHECK (opening_cash_known IN (0,1)),
  activity_cash INTEGER NOT NULL DEFAULT 0,
  cash_left INTEGER CHECK (cash_left IS NULL OR cash_left>=0),
  withdrawn_cash INTEGER CHECK (withdrawn_cash IS NULL OR withdrawn_cash>=0),
  withdrawal_destination TEXT CHECK (withdrawal_destination IS NULL OR withdrawal_destination='gym_fund'),
  correction_reason TEXT,
  adjusted_after_withdrawal INTEGER NOT NULL DEFAULT 0 CHECK (adjusted_after_withdrawal IN (0,1)),
  integrity_note TEXT,
  opened_at INTEGER NOT NULL,
  opened_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  closed_at INTEGER,
  reconciled_at INTEGER,
  reconciled_by TEXT REFERENCES users(id) ON DELETE RESTRICT,
  stale_at INTEGER,
  withdrawn_at INTEGER,
  withdrawn_by TEXT REFERENCES users(id) ON DELETE RESTRICT,
  FOREIGN KEY(gym_id,drawer_id) REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT,
  CHECK (withdrawn_cash IS NULL OR
    (counted_cash IS NOT NULL AND cash_left IS NOT NULL AND withdrawn_cash=counted_cash-cash_left)),
  CHECK (adjusted_after_withdrawal=0 OR
    (status='withdrawn' AND integrity_note IS NOT NULL AND length(trim(integrity_note))>0)),
  CHECK (
    (status='open' AND closed_at IS NULL AND counted_cash IS NULL AND withdrawn_at IS NULL)
    OR (status='closed_unverified' AND closed_at IS NOT NULL AND counted_cash IS NULL AND withdrawn_at IS NULL)
    OR (status='reconciled' AND closed_at IS NOT NULL AND counted_cash IS NOT NULL
        AND reconciled_at IS NOT NULL AND reconciled_by IS NOT NULL AND withdrawn_at IS NULL)
    OR (status='stale' AND closed_at IS NOT NULL AND stale_at IS NOT NULL
        AND correction_reason IS NOT NULL AND length(trim(correction_reason))>0 AND withdrawn_at IS NULL)
    OR (status='withdrawn' AND closed_at IS NOT NULL AND counted_cash IS NOT NULL
        AND reconciled_at IS NOT NULL AND reconciled_by IS NOT NULL
        AND cash_left IS NOT NULL AND withdrawn_cash IS NOT NULL
        AND withdrawal_destination='gym_fund' AND withdrawn_at IS NOT NULL AND withdrawn_by IS NOT NULL)
  )
);
INSERT INTO cash_close_events(
  id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
  close_date,calculated_cash,counted_cash,discrepancy_reason,closed_by,
  drawer_id,drawer_code,operational_date,sequence,status,opening_cash,
  opening_cash_known,activity_cash,cash_left,withdrawn_cash,
  withdrawal_destination,correction_reason,adjusted_after_withdrawal,
  integrity_note,opened_at,opened_by,closed_at,reconciled_at,reconciled_by,
  stale_at,withdrawn_at,withdrawn_by)
SELECT
  id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
  close_date,calculated_cash,counted_cash,discrepancy_reason,closed_by,
  drawer_id,drawer_code,operational_date,sequence,status,opening_cash,
  opening_cash_known,activity_cash,cash_left,withdrawn_cash,
  withdrawal_destination,correction_reason,adjusted_after_withdrawal,
  integrity_note,opened_at,opened_by,closed_at,reconciled_at,reconciled_by,
  stale_at,withdrawn_at,withdrawn_by
FROM cash_close_events_legacy_035;
DROP TABLE cash_close_events_legacy_035;

CREATE UNIQUE INDEX uq_cash_sessions_natural
  ON cash_close_events(gym_id, drawer_id, operational_date, sequence)
  WHERE deleted_at IS NULL;
CREATE INDEX idx_cash_sessions_gym_day
  ON cash_close_events(gym_id, operational_date, drawer_id, sequence DESC)
  WHERE deleted_at IS NULL;
CREATE INDEX idx_cash_close_events_sync ON cash_close_events(gym_id,updated_at);

CREATE TABLE IF NOT EXISTS cash_transfers (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER,
  synced_at INTEGER,
  session_id TEXT NOT NULL REFERENCES cash_close_events(id) ON UPDATE CASCADE ON DELETE RESTRICT,
  drawer_id TEXT NOT NULL,
  destination TEXT NOT NULL CHECK (destination = 'gym_fund'),
  amount INTEGER NOT NULL CHECK (amount >= 0),
  transferred_at INTEGER NOT NULL,
  transferred_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  FOREIGN KEY(gym_id,drawer_id) REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_transfers_session
  ON cash_transfers(session_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_cash_transfers_gym_time
  ON cash_transfers(gym_id, transferred_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_cash_transfers_sync
  ON cash_transfers(gym_id, updated_at);

UPDATE sync_queue
SET payload = json_set(payload,
  '$.drawer_id', COALESCE(json_extract(payload, '$.drawer_id'), json_extract(payload, '$.gym_id')),
  '$.drawer_code', COALESCE(json_extract(payload, '$.drawer_code'), 'main'),
  '$.operational_date', COALESCE(json_extract(payload, '$.operational_date'), json_extract(payload, '$.close_date')),
  '$.sequence', COALESCE(json_extract(payload, '$.sequence'), 1),
  '$.status', COALESCE(json_extract(payload, '$.status'), CASE WHEN json_extract(payload, '$.counted_cash') IS NULL THEN 'closed_unverified' ELSE 'reconciled' END),
  '$.opening_cash', COALESCE(json_extract(payload, '$.opening_cash'), 0),
  '$.opening_cash_known', COALESCE(json_extract(payload, '$.opening_cash_known'), 0),
  '$.activity_cash', COALESCE(json_extract(payload, '$.activity_cash'), json_extract(payload, '$.calculated_cash')),
  '$.adjusted_after_withdrawal', COALESCE(json_extract(payload, '$.adjusted_after_withdrawal'), 0),
  '$.opened_at', COALESCE(json_extract(payload, '$.opened_at'),
    CAST(strftime('%s', COALESCE(json_extract(payload, '$.operational_date'), json_extract(payload, '$.close_date')) || ' 00:00:00') AS INTEGER) * 1000),
  '$.opened_by', COALESCE(json_extract(payload, '$.opened_by'), json_extract(payload, '$.closed_by')),
  '$.closed_at', COALESCE(json_extract(payload, '$.closed_at'), json_extract(payload, '$.created_at')),
  '$.reconciled_at', CASE WHEN json_extract(payload, '$.counted_cash') IS NULL THEN NULL ELSE COALESCE(json_extract(payload, '$.reconciled_at'), json_extract(payload, '$.created_at')) END,
  '$.reconciled_by', CASE WHEN json_extract(payload, '$.counted_cash') IS NULL THEN NULL ELSE COALESCE(json_extract(payload, '$.reconciled_by'), json_extract(payload, '$.closed_by')) END
)
WHERE entity_type = 'cash_close_events' AND synced_at IS NULL AND json_valid(payload);
UPDATE sync_queue SET payload=json_set(payload,'$.cash_drawer_id',
  COALESCE(json_extract(payload,'$.cash_drawer_id'),json_extract(payload,'$.gym_id')))
WHERE entity_type IN ('payments','cash_movements') AND synced_at IS NULL AND json_valid(payload);

INSERT INTO _migrations(version, name, applied_at)
SELECT 35, '035_cash_sessions', CAST(strftime('%s','now') AS INTEGER) * 1000
WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version = 35);

COMMIT;
