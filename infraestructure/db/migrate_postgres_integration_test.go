//go:build server && integration

package db_test

import (
	"github.com/google/uuid"
	"testing"

	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// This contract protects the compatibility delta in PG 047. It deliberately
// checks the resulting catalog instead of only trusting _migrations: the bug
// it prevents was precisely a migration marker whose historical file no
// longer described the schema that had actually been applied.
func TestPostgresFinancialSchemaContract(t *testing.T) {
	db := testutil.OpenPostgres(t)

	type missingColumn struct {
		TableName  string
		ColumnName string
	}
	var missing []missingColumn
	if err := db.Raw(`
		WITH required(table_name,column_name) AS (VALUES
		  ('sales','correction_version'),
		  ('payments','recognized_amount'),
		  ('payments','membership_id'),
		  ('payments','idempotency_key'),
		  ('payments','idempotency_fingerprint'),
		  ('payments','idempotency_result'),
		  ('payments','cash_drawer_id'),
		  ('refunds','kind'),
		  ('cash_drawers','id'),
		  ('cash_movements','cash_drawer_id'),
		  ('cash_close_events','adjusted_after_withdrawal'),
		  ('cash_close_events','integrity_note')
		)
		SELECT r.table_name,r.column_name
		FROM required r
		LEFT JOIN information_schema.columns c
		  ON c.table_schema='public' AND c.table_name=r.table_name AND c.column_name=r.column_name
		WHERE c.column_name IS NULL
		ORDER BY r.table_name,r.column_name`).Scan(&missing).Error; err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing financial schema columns: %+v", missing)
	}

	var missingObjects []string
	if err := db.Raw(`
		WITH required(kind,name) AS (VALUES
		  ('constraint','chk_sales_correction_version'),
		  ('constraint','chk_payments_recognized_amount'),
		  ('constraint','chk_payments_idempotency_shape'),
		  ('constraint','fk_payments_cash_drawer'),
		  ('constraint','fk_cash_movements_drawer'),
		  ('constraint','fk_cash_sessions_drawer'),
		  ('constraint','fk_cash_transfers_drawer'),
		  ('constraint','chk_cash_sessions_adjustment_shape'),
		  ('constraint','chk_cash_sessions_lifecycle'),
		  ('index','uq_payments_idempotency'),
		  ('index','idx_payments_cash_drawer_session'),
		  ('index','idx_cash_movements_drawer_session'),
		  ('trigger','trg_gyms_seed_main_cash_drawer')
		), present(kind,name) AS (
		  SELECT 'constraint',conname FROM pg_constraint WHERE connamespace='public'::regnamespace
		  UNION ALL SELECT 'index',indexname FROM pg_indexes WHERE schemaname='public'
		  UNION ALL SELECT 'trigger',tgname FROM pg_trigger WHERE NOT tgisinternal
		)
		SELECT r.kind || ':' || r.name
		FROM required r LEFT JOIN present p ON p.kind=r.kind AND p.name=r.name
		WHERE p.name IS NULL ORDER BY r.kind,r.name`).Scan(&missingObjects).Error; err != nil {
		t.Fatal(err)
	}
	if len(missingObjects) != 0 {
		t.Fatalf("missing financial schema objects: %v", missingObjects)
	}

	// Other packages create and tear down fixtures concurrently. Check the
	// trigger on our own transaction instead of observing another test
	// between its DELETE cash_drawers and DELETE gyms cleanup statements.
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	gymID := uuid.New()
	if err := tx.Exec(`INSERT INTO gyms (id,gym_id,version,created_at,updated_at,name,country,timezone)
      VALUES (?,?,1,NOW(),NOW(),'Financial schema fixture','MX','America/Mexico_City')`, gymID, gymID).Error; err != nil {
		t.Fatal(err)
	}
	var gymsWithoutMainDrawer int64
	if err := tx.Raw(`
		SELECT COUNT(*) FROM gyms g
		LEFT JOIN cash_drawers d
		  ON d.id=g.id AND d.gym_id=g.id AND d.is_main=TRUE AND d.deleted_at IS NULL
		WHERE g.id=? AND d.id IS NULL`, gymID).Scan(&gymsWithoutMainDrawer).Error; err != nil {
		t.Fatal(err)
	}
	if gymsWithoutMainDrawer != 0 {
		t.Fatalf("gyms without deterministic main drawer: %d", gymsWithoutMainDrawer)
	}
}
