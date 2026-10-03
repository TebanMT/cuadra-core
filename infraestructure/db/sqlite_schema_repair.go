//go:build sidecar

package db

import (
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
)

// repairSQLiteSchemaDrift reconciles development builds that recorded a
// migration while that migration file was still being completed. SQLite's
// runner correctly skips recorded numeric versions, but that means editing an
// already-applied file cannot upgrade the existing database. Released SQL
// migrations remain immutable; these guarded repairs only bridge the affected
// pre-release installations without deleting their local operation data.
func repairSQLiteSchemaDrift(db *sqlx.DB, applied map[int]struct{}) error {
	if _, ok := applied[34]; ok {
		if err := repairSQLiteFinancialIntegrityDrift(db, applied); err != nil {
			return err
		}
	}
	if _, ok := applied[35]; ok {
		if err := repairSQLiteCashSessionDrift(db); err != nil {
			return err
		}
	}
	if _, ok := applied[38]; ok {
		if err := repairSQLiteSaleCorrectionDrift(db); err != nil {
			return err
		}
	}
	return nil
}

func repairSQLiteFinancialIntegrityDrift(db *sqlx.DB, applied map[int]struct{}) error {
	if exists, err := sqliteTableExists(db, "payments"); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("repair sqlite migration 34: payments table is missing")
	}

	paymentColumns := []struct {
		name, definition string
	}{
		{"membership_id", "TEXT REFERENCES memberships(id) ON DELETE RESTRICT"},
		{"idempotency_key", "TEXT"},
		{"idempotency_fingerprint", "TEXT"},
		{"idempotency_result", "TEXT CHECK(idempotency_result IS NULL OR json_valid(idempotency_result))"},
		{"recognized_amount", "INTEGER NOT NULL DEFAULT 0 CHECK(recognized_amount>=0)"},
	}
	for _, column := range paymentColumns {
		added, err := ensureSQLiteColumn(db, "payments", column.name, column.definition)
		if err != nil {
			return fmt.Errorf("repair sqlite payments.%s: %w", column.name, err)
		}
		if column.name == "recognized_amount" && added {
			// Same deterministic backfill as final v34. Before v34, positive
			// payment amount was recognized income and refund rows were tracked
			// separately by the refunds journal.
			if _, err := db.Exec(`UPDATE payments SET recognized_amount=
				CASE WHEN concept='refund' THEN 0 ELSE amount END`); err != nil {
				return fmt.Errorf("backfill sqlite payments.recognized_amount: %w", err)
			}
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_payments_idempotency
		ON payments(gym_id,idempotency_key)
		WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL`); err != nil {
		return fmt.Errorf("repair sqlite payments idempotency index: %w", err)
	}
	// The drifted table still permits the removed fictitious "credit" rail
	// in its old CHECK. These triggers give all new writes the final v34 rule
	// without rebuilding 900+ payment rows and their dependent graph.
	if _, err := db.Exec(`
		CREATE TRIGGER IF NOT EXISTS trg_payments_final_method_insert_034
		BEFORE INSERT ON payments
		WHEN NEW.payment_method NOT IN ('cash','transfer','card')
		BEGIN SELECT RAISE(ABORT,'invalid payment method'); END;
		CREATE TRIGGER IF NOT EXISTS trg_payments_final_method_update_034
		BEFORE UPDATE OF payment_method ON payments
		WHEN NEW.payment_method NOT IN ('cash','transfer','card')
		BEGIN SELECT RAISE(ABORT,'invalid payment method'); END;
		CREATE TRIGGER IF NOT EXISTS trg_payments_idempotency_insert_034
		BEFORE INSERT ON payments
		WHEN NOT (
		  (NEW.idempotency_key IS NULL AND NEW.idempotency_fingerprint IS NULL AND NEW.idempotency_result IS NULL)
		  OR (length(trim(NEW.idempotency_key)) BETWEEN 1 AND 120
		      AND length(trim(NEW.idempotency_fingerprint))>0))
		BEGIN SELECT RAISE(ABORT,'invalid payment idempotency metadata'); END;
		CREATE TRIGGER IF NOT EXISTS trg_payments_idempotency_update_034
		BEFORE UPDATE OF idempotency_key,idempotency_fingerprint,idempotency_result ON payments
		WHEN NOT (
		  (NEW.idempotency_key IS NULL AND NEW.idempotency_fingerprint IS NULL AND NEW.idempotency_result IS NULL)
		  OR (length(trim(NEW.idempotency_key)) BETWEEN 1 AND 120
		      AND length(trim(NEW.idempotency_fingerprint))>0))
		BEGIN SELECT RAISE(ABORT,'invalid payment idempotency metadata'); END;`); err != nil {
		return fmt.Errorf("repair sqlite payments write barriers: %w", err)
	}

	if exists, err := sqliteTableExists(db, "refunds"); err != nil {
		return err
	} else if exists {
		if _, err := ensureSQLiteColumn(db, "refunds", "kind", `TEXT NOT NULL DEFAULT 'revenue_refund'
			CHECK(kind IN ('revenue_refund','overcollection_settlement'))`); err != nil {
			return fmt.Errorf("repair sqlite refunds.kind: %w", err)
		}
		if _, idempotencyApplied := applied[36]; idempotencyApplied {
			if _, err := ensureSQLiteColumn(db, "refunds", "idempotency_fingerprint", "TEXT"); err != nil {
				return fmt.Errorf("repair sqlite refunds.idempotency_fingerprint: %w", err)
			}
			if _, err := ensureSQLiteColumn(db, "refunds", "idempotency_result",
				"TEXT CHECK(idempotency_result IS NULL OR json_valid(idempotency_result))"); err != nil {
				return fmt.Errorf("repair sqlite refunds.idempotency_result: %w", err)
			}
			if _, err := db.Exec(`UPDATE refunds SET idempotency_fingerprint='legacy:'||id
				WHERE idempotency_fingerprint IS NULL OR trim(idempotency_fingerprint)=''`); err != nil {
				return fmt.Errorf("backfill sqlite refund idempotency: %w", err)
			}
			if err := repairSQLiteRefundNullability(db); err != nil {
				return err
			}
		}
	}
	return nil
}

func repairSQLiteCashSessionDrift(db *sqlx.DB) error {
	if _, err := db.Exec(sqliteCashDrawersSchema); err != nil {
		return fmt.Errorf("repair sqlite cash_drawers: %w", err)
	}
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO cash_drawers(
			id,gym_id,version,created_at,updated_at,code,name,active,is_main)
		SELECT id,id,1,created_at,updated_at,'main','Caja principal',1,1 FROM gyms;
		CREATE TRIGGER IF NOT EXISTS trg_gyms_seed_main_cash_drawer
		AFTER INSERT ON gyms BEGIN
		  INSERT OR IGNORE INTO cash_drawers(
		    id,gym_id,version,created_at,updated_at,code,name,active,is_main)
		  VALUES(NEW.id,NEW.id,1,NEW.created_at,NEW.updated_at,'main','Caja principal',1,1);
		END;`); err != nil {
		return fmt.Errorf("repair sqlite main cash drawers: %w", err)
	}

	if exists, err := sqliteTableExists(db, "payments"); err != nil {
		return err
	} else if exists {
		if _, err := ensureSQLiteColumn(db, "payments", "cash_drawer_id", "TEXT"); err != nil {
			return fmt.Errorf("repair sqlite payments.cash_drawer_id: %w", err)
		}
	}
	if exists, err := sqliteTableExists(db, "cash_movements"); err != nil {
		return err
	} else if exists {
		if _, err := ensureSQLiteColumn(db, "cash_movements", "cash_drawer_id", "TEXT"); err != nil {
			return fmt.Errorf("repair sqlite cash_movements.cash_drawer_id: %w", err)
		}
	}
	// Older schemas have no destination column. Preserve outside-cash income
	// when reconciling a current database on subsequent starts.
	var hasDestination int
	if err := db.Get(&hasDestination, `SELECT COUNT(*) FROM pragma_table_info('payments') WHERE name='cash_destination'`); err != nil {
		return err
	}
	cashDestinationFilter := ""
	if hasDestination != 0 {
		cashDestinationFilter = " AND cash_destination='cash_drawer'"
	}
	if _, err := db.Exec(`
		UPDATE payments SET cash_drawer_id=gym_id
		 WHERE cash_drawer_id IS NULL AND payment_method='cash'` + cashDestinationFilter + `;
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
		  ON payments(gym_id,cash_drawer_id,payment_date,created_at)
		  WHERE deleted_at IS NULL AND payment_method='cash';
		CREATE INDEX IF NOT EXISTS idx_cash_movements_drawer_session
		  ON cash_movements(gym_id,cash_drawer_id,movement_on,created_at)
		  WHERE deleted_at IS NULL;`); err != nil {
		return fmt.Errorf("repair sqlite cash drawer attribution: %w", err)
	}

	if err := repairSQLiteCashCloseTable(db); err != nil {
		return err
	}
	if _, err := db.Exec(`
		UPDATE sync_queue SET payload=json_set(payload,'$.cash_drawer_id',
		  COALESCE(json_extract(payload,'$.cash_drawer_id'),json_extract(payload,'$.gym_id')))
		WHERE entity_type IN ('payments','cash_movements')
		  AND synced_at IS NULL AND json_valid(payload)
		  AND json_extract(payload,'$.cash_drawer_id') IS NULL
		  AND (entity_type='cash_movements' OR (
		    json_extract(payload,'$.payment_method')='cash'
		    AND COALESCE(json_extract(payload,'$.cash_destination'),'cash_drawer')='cash_drawer'))`); err != nil {
		return fmt.Errorf("repair sqlite pending cash drawer payloads: %w", err)
	}
	return nil
}

func repairSQLiteSaleCorrectionDrift(db *sqlx.DB) error {
	if exists, err := sqliteTableExists(db, "sale_corrections"); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("repair sqlite migration 38: sale_corrections table is missing")
	}
	columns := []struct{ name, definition string }{
		{"idempotency_fingerprint", "TEXT"},
		{"idempotency_result", "TEXT CHECK(idempotency_result IS NULL OR json_valid(idempotency_result))"},
		{"correction_type", "TEXT NOT NULL DEFAULT 'edit' CHECK(correction_type IN ('edit','annul'))"},
		{"increase_resolution", "TEXT CHECK(increase_resolution IS NULL OR increase_resolution IN ('pending','already_collected','collect_now'))"},
	}
	for _, column := range columns {
		if _, err := ensureSQLiteColumn(db, "sale_corrections", column.name, column.definition); err != nil {
			return fmt.Errorf("repair sqlite sale_corrections.%s: %w", column.name, err)
		}
	}
	if _, err := db.Exec(`UPDATE sale_corrections SET idempotency_fingerprint='legacy:'||id
		WHERE idempotency_fingerprint IS NULL OR trim(idempotency_fingerprint)=''`); err != nil {
		return fmt.Errorf("backfill sqlite sale correction idempotency: %w", err)
	}
	return nil
}

func repairSQLiteCashCloseTable(db *sqlx.DB) error {
	exists, err := sqliteTableExists(db, "cash_close_events")
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("repair sqlite migration 35: cash_close_events table is missing")
	}
	columns := []struct{ name, definition string }{
		{"drawer_id", "TEXT"}, {"drawer_code", "TEXT"}, {"operational_date", "TEXT"},
		{"sequence", "INTEGER"}, {"status", "TEXT"}, {"opening_cash", "INTEGER"},
		{"opening_cash_known", "INTEGER"}, {"activity_cash", "INTEGER"}, {"cash_left", "INTEGER"},
		{"withdrawn_cash", "INTEGER"}, {"withdrawal_destination", "TEXT"}, {"correction_reason", "TEXT"},
		{"adjusted_after_withdrawal", "INTEGER"}, {"integrity_note", "TEXT"}, {"opened_at", "INTEGER"},
		{"opened_by", "TEXT"}, {"closed_at", "INTEGER"}, {"reconciled_at", "INTEGER"},
		{"reconciled_by", "TEXT"}, {"stale_at", "INTEGER"}, {"withdrawn_at", "INTEGER"},
		{"withdrawn_by", "TEXT"},
	}
	for _, column := range columns {
		if _, err := ensureSQLiteColumn(db, "cash_close_events", column.name, column.definition); err != nil {
			return fmt.Errorf("repair sqlite cash_close_events.%s: %w", column.name, err)
		}
	}
	if _, err := db.Exec(`UPDATE cash_close_events SET
		drawer_id=COALESCE(drawer_id,gym_id),
		drawer_code=COALESCE(NULLIF(drawer_code,''),'main'),
		operational_date=COALESCE(operational_date,close_date),
		sequence=COALESCE(sequence,1),
		status=COALESCE(status,CASE WHEN counted_cash IS NULL THEN 'closed_unverified' ELSE 'reconciled' END),
		opening_cash=COALESCE(opening_cash,0), opening_cash_known=COALESCE(opening_cash_known,0),
		activity_cash=COALESCE(activity_cash,calculated_cash),
		adjusted_after_withdrawal=COALESCE(adjusted_after_withdrawal,0),
		opened_at=COALESCE(opened_at,CAST(strftime('%s',COALESCE(operational_date,close_date)||' 00:00:00') AS INTEGER)*1000),
		opened_by=COALESCE(opened_by,closed_by),
		closed_at=CASE WHEN status='open' THEN closed_at ELSE COALESCE(closed_at,created_at) END,
		reconciled_at=CASE WHEN counted_cash IS NULL THEN reconciled_at ELSE COALESCE(reconciled_at,created_at) END,
		reconciled_by=CASE WHEN counted_cash IS NULL THEN reconciled_by ELSE COALESCE(reconciled_by,closed_by) END`); err != nil {
		return fmt.Errorf("backfill sqlite cash sessions: %w", err)
	}

	var tableSQL string
	if err := db.Get(&tableSQL, `SELECT sql FROM sqlite_master
		WHERE type='table' AND name='cash_close_events'`); err != nil {
		return fmt.Errorf("inspect sqlite cash_close_events schema: %w", err)
	}
	if strings.Contains(tableSQL, "adjusted_after_withdrawal=0") &&
		strings.Contains(tableSQL, "FOREIGN KEY(gym_id,drawer_id)") {
		if _, err := db.Exec(sqliteCashTransferIndexes); err != nil {
			return fmt.Errorf("repair sqlite cash transfer indexes: %w", err)
		}
		return nil
	}

	return withSQLiteForeignKeysDisabled(db, func(tx *sqlx.Tx) error {
		var transfersExist int
		if err := tx.Get(&transfersExist, `SELECT COUNT(*) FROM sqlite_master
			WHERE type='table' AND name='cash_transfers'`); err != nil {
			return err
		}
		if transfersExist != 0 {
			if _, err := tx.Exec(`ALTER TABLE cash_transfers RENAME TO cash_transfers_repair_035`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`ALTER TABLE cash_close_events RENAME TO cash_close_events_repair_035`); err != nil {
			return err
		}
		if _, err := tx.Exec(sqliteCashCloseSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO cash_close_events(
			id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
			close_date,calculated_cash,counted_cash,discrepancy_reason,closed_by,
			drawer_id,drawer_code,operational_date,sequence,status,opening_cash,
			opening_cash_known,activity_cash,cash_left,withdrawn_cash,withdrawal_destination,
			correction_reason,adjusted_after_withdrawal,integrity_note,opened_at,opened_by,
			closed_at,reconciled_at,reconciled_by,stale_at,withdrawn_at,withdrawn_by)
			SELECT id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
			close_date,calculated_cash,counted_cash,discrepancy_reason,closed_by,
			drawer_id,drawer_code,operational_date,sequence,status,opening_cash,
			opening_cash_known,activity_cash,cash_left,withdrawn_cash,withdrawal_destination,
			correction_reason,adjusted_after_withdrawal,integrity_note,opened_at,opened_by,
			closed_at,reconciled_at,reconciled_by,stale_at,withdrawn_at,withdrawn_by
			FROM cash_close_events_repair_035`); err != nil {
			return err
		}
		if _, err := tx.Exec(sqliteCashTransfersSchema); err != nil {
			return err
		}
		if transfersExist != 0 {
			if _, err := tx.Exec(`INSERT INTO cash_transfers
				SELECT * FROM cash_transfers_repair_035`); err != nil {
				return err
			}
			if _, err := tx.Exec(`DROP TABLE cash_transfers_repair_035`); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DROP TABLE cash_close_events_repair_035`); err != nil {
			return err
		}
		_, err := tx.Exec(sqliteCashSessionIndexes + sqliteCashTransferIndexes)
		return err
	})
}

func repairSQLiteRefundNullability(db *sqlx.DB) error {
	var strictColumns int
	if err := db.Get(&strictColumns, `SELECT COUNT(*) FROM pragma_table_info('refunds')
		WHERE name IN ('refund_payment_id','method') AND "notnull"=1`); err != nil {
		return fmt.Errorf("inspect sqlite refund nullability: %w", err)
	}
	if strictColumns == 0 {
		return nil
	}
	return withSQLiteForeignKeysDisabled(db, func(tx *sqlx.Tx) error {
		if _, err := tx.Exec(`ALTER TABLE refunds RENAME TO refunds_repair_034`); err != nil {
			return err
		}
		if _, err := tx.Exec(sqliteRefundsSchema); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO refunds(
			id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
			root_payment_id,refund_payment_id,sale_id,amount,method,refunded_on,reason,kind,
			balance_cancelled,legacy_incomplete,correction_id,idempotency_key,
			idempotency_fingerprint,idempotency_result,created_by)
			SELECT id,gym_id,version,created_at,updated_at,deleted_at,synced_at,
			root_payment_id,refund_payment_id,sale_id,amount,method,refunded_on,reason,kind,
			balance_cancelled,legacy_incomplete,correction_id,idempotency_key,
			idempotency_fingerprint,idempotency_result,created_by
			FROM refunds_repair_034`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DROP TABLE refunds_repair_034`); err != nil {
			return err
		}
		_, err := tx.Exec(sqliteRefundIndexesAndTrigger)
		return err
	})
}

func sqliteTableExists(db sqlx.Queryer, table string) (bool, error) {
	var count int
	if err := sqlx.Get(db, &count, `SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name=?`, table); err != nil {
		return false, fmt.Errorf("inspect sqlite table %s: %w", table, err)
	}
	return count != 0, nil
}

func ensureSQLiteColumn(db *sqlx.DB, table, column, definition string) (bool, error) {
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column); err != nil {
		// SQLite does not bind the pragma table argument on every supported
		// version; fall back to the quoted, internally controlled table name.
		query := fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name=?", table)
		if fallbackErr := db.Get(&count, query, column); fallbackErr != nil {
			return false, fallbackErr
		}
	}
	if count != 0 {
		return false, nil
	}
	query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)
	if _, err := db.Exec(query); err != nil {
		return false, err
	}
	return true, nil
}

func withSQLiteForeignKeysDisabled(db *sqlx.DB, change func(*sqlx.Tx) error) error {
	var foreignKeys, legacyAlter int
	if err := db.Get(&foreignKeys, `PRAGMA foreign_keys`); err != nil {
		return err
	}
	if err := db.Get(&legacyAlter, `PRAGMA legacy_alter_table`); err != nil {
		return err
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	if _, err := db.Exec(`PRAGMA legacy_alter_table=ON`); err != nil {
		return err
	}
	tx, err := db.Beginx()
	if err == nil {
		err = change(tx)
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
	} else {
		err = tx.Commit()
	}
	if legacyAlter == 0 {
		if _, restoreErr := db.Exec(`PRAGMA legacy_alter_table=OFF`); err == nil && restoreErr != nil {
			err = restoreErr
		}
	}
	if foreignKeys != 0 {
		if _, restoreErr := db.Exec(`PRAGMA foreign_keys=ON`); err == nil && restoreErr != nil {
			err = restoreErr
		}
	}
	if err != nil {
		return fmt.Errorf("rebuild drifted sqlite schema: %w", err)
	}
	return nil
}

const sqliteCashDrawersSchema = `
CREATE TABLE IF NOT EXISTS cash_drawers (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1 CHECK(version>0),
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  deleted_at INTEGER, synced_at INTEGER,
  code TEXT NOT NULL CHECK(length(trim(code)) BETWEEN 1 AND 40),
  name TEXT NOT NULL CHECK(length(trim(name)) BETWEEN 1 AND 60),
  active INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0,1)),
  is_main INTEGER NOT NULL DEFAULT 0 CHECK(is_main IN (0,1)),
  idempotency_key TEXT CHECK(idempotency_key IS NULL OR length(trim(idempotency_key)) BETWEEN 1 AND 120),
  CHECK((is_main=1 AND id=gym_id AND code='main' AND active=1) OR
        (is_main=0 AND id<>gym_id AND code<>'main')),
  UNIQUE(gym_id,id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_name ON cash_drawers(gym_id,lower(name)) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_code ON cash_drawers(gym_id,lower(code)) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_main ON cash_drawers(gym_id) WHERE is_main=1 AND deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_drawers_idempotency ON cash_drawers(gym_id,idempotency_key)
  WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_cash_drawers_sync ON cash_drawers(gym_id,updated_at);`

const sqliteCashCloseSchema = `CREATE TABLE cash_close_events (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  deleted_at INTEGER, synced_at INTEGER,
  close_date TEXT NOT NULL, calculated_cash INTEGER NOT NULL,
  counted_cash INTEGER CHECK(counted_cash IS NULL OR counted_cash>=0),
  discrepancy INTEGER GENERATED ALWAYS AS (
    CASE WHEN status IN ('reconciled','withdrawn') AND counted_cash IS NOT NULL
              AND adjusted_after_withdrawal=0
      THEN counted_cash-calculated_cash ELSE NULL END
  ) STORED,
  discrepancy_reason TEXT,
  closed_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  drawer_id TEXT NOT NULL,
  drawer_code TEXT NOT NULL DEFAULT 'main' CHECK(length(trim(drawer_code)) BETWEEN 1 AND 40),
  operational_date TEXT NOT NULL,
  sequence INTEGER NOT NULL DEFAULT 1 CHECK(sequence>0),
  status TEXT NOT NULL DEFAULT 'open' CHECK(status IN ('open','closed_unverified','reconciled','stale','withdrawn')),
  opening_cash INTEGER NOT NULL DEFAULT 0 CHECK(opening_cash>=0),
  opening_cash_known INTEGER NOT NULL DEFAULT 1 CHECK(opening_cash_known IN (0,1)),
  activity_cash INTEGER NOT NULL DEFAULT 0,
  cash_left INTEGER CHECK(cash_left IS NULL OR cash_left>=0),
  withdrawn_cash INTEGER CHECK(withdrawn_cash IS NULL OR withdrawn_cash>=0),
  withdrawal_destination TEXT CHECK(withdrawal_destination IS NULL OR withdrawal_destination='gym_fund'),
  correction_reason TEXT,
  adjusted_after_withdrawal INTEGER NOT NULL DEFAULT 0 CHECK(adjusted_after_withdrawal IN (0,1)),
  integrity_note TEXT,
  opened_at INTEGER NOT NULL,
  opened_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  closed_at INTEGER, reconciled_at INTEGER,
  reconciled_by TEXT REFERENCES users(id) ON DELETE RESTRICT,
  stale_at INTEGER, withdrawn_at INTEGER,
  withdrawn_by TEXT REFERENCES users(id) ON DELETE RESTRICT,
  FOREIGN KEY(gym_id,drawer_id) REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT,
  CHECK(withdrawn_cash IS NULL OR
    (counted_cash IS NOT NULL AND cash_left IS NOT NULL AND withdrawn_cash=counted_cash-cash_left)),
  CHECK(adjusted_after_withdrawal=0 OR
    (status='withdrawn' AND integrity_note IS NOT NULL AND length(trim(integrity_note))>0)),
  CHECK(
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
);`

const sqliteCashTransfersSchema = `CREATE TABLE IF NOT EXISTS cash_transfers (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  deleted_at INTEGER, synced_at INTEGER,
  session_id TEXT NOT NULL REFERENCES cash_close_events(id) ON UPDATE CASCADE ON DELETE RESTRICT,
  drawer_id TEXT NOT NULL,
  destination TEXT NOT NULL CHECK(destination='gym_fund'),
  amount INTEGER NOT NULL CHECK(amount>=0),
  transferred_at INTEGER NOT NULL,
  transferred_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  FOREIGN KEY(gym_id,drawer_id) REFERENCES cash_drawers(gym_id,id) ON DELETE RESTRICT
);`

const sqliteCashSessionIndexes = `
CREATE UNIQUE INDEX uq_cash_sessions_natural
  ON cash_close_events(gym_id,drawer_id,operational_date,sequence) WHERE deleted_at IS NULL;
CREATE INDEX idx_cash_sessions_gym_day
  ON cash_close_events(gym_id,operational_date,drawer_id,sequence DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_cash_close_events_sync ON cash_close_events(gym_id,updated_at);`

const sqliteCashTransferIndexes = `
CREATE UNIQUE INDEX IF NOT EXISTS uq_cash_transfers_session ON cash_transfers(session_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_cash_transfers_gym_time ON cash_transfers(gym_id,transferred_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_cash_transfers_sync ON cash_transfers(gym_id,updated_at);`

const sqliteRefundsSchema = `CREATE TABLE refunds (
  id TEXT PRIMARY KEY,
  gym_id TEXT NOT NULL REFERENCES gyms(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  deleted_at INTEGER, synced_at INTEGER,
  root_payment_id TEXT NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
  refund_payment_id TEXT UNIQUE REFERENCES payments(id) ON DELETE RESTRICT,
  sale_id TEXT REFERENCES sales(id) ON DELETE RESTRICT,
  amount INTEGER NOT NULL CHECK(amount>=0),
  method TEXT CHECK(method IN ('cash','transfer','card')),
  refunded_on TEXT NOT NULL,
  reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 200),
  kind TEXT NOT NULL DEFAULT 'revenue_refund' CHECK(kind IN ('revenue_refund','overcollection_settlement')),
  balance_cancelled INTEGER NOT NULL DEFAULT 0 CHECK(balance_cancelled>=0),
  legacy_incomplete INTEGER NOT NULL DEFAULT 0,
  correction_id TEXT,
  idempotency_key TEXT NOT NULL,
  idempotency_fingerprint TEXT,
  idempotency_result TEXT CHECK(idempotency_result IS NULL OR json_valid(idempotency_result)),
  created_by TEXT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  CHECK((amount>0 AND method IS NOT NULL) OR (amount=0 AND balance_cancelled>0 AND method IS NULL)),
  UNIQUE(gym_id,idempotency_key)
);`

const sqliteRefundIndexesAndTrigger = `
CREATE INDEX idx_refunds_root ON refunds(root_payment_id,refunded_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refunds_correction_kind ON refunds(correction_id,kind,refunded_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refunds_gym_date ON refunds(gym_id,refunded_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_refunds_sync ON refunds(gym_id,updated_at);
CREATE TRIGGER refunds_correction_fk_insert
BEFORE INSERT ON refunds
WHEN NEW.correction_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM sale_corrections WHERE id=NEW.correction_id)
BEGIN SELECT RAISE(ABORT,'invalid refund correction'); END;`
