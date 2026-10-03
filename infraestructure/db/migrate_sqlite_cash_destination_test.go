//go:build sidecar

package db

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/jmoiron/sqlx"
)

func sqliteBeforeCashDestination(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite3", filepath.Join(t.TempDir(), "upgrade.db")+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	older := fstest.MapFS{}
	files, err := fs.ReadDir(os.DirFS("../../db_migrations"), "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if v, ok := sqliteVersionFromFilename(file.Name()); ok && v <= 42 {
			raw, err := os.ReadFile("../../db_migrations/sqlite/" + file.Name())
			if err != nil {
				t.Fatal(err)
			}
			older["sqlite/"+file.Name()] = &fstest.MapFile{Data: raw}
		}
	}
	if err := ApplySQLiteMigrations(db, older, "sqlite"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestApplySQLiteMigrations_ExistingCashDestinationWithoutMarker(t *testing.T) {
	db := sqliteBeforeCashDestination(t)
	// An earlier development build added the exact final column, but did not
	// record v43. Reproduce a non-empty installation, including unsent work.
	_, err := db.Exec(`
		ALTER TABLE payments ADD COLUMN cash_destination TEXT NOT NULL DEFAULT 'cash_drawer'
		  CHECK(cash_destination IN ('cash_drawer','gym_fund'));
		INSERT INTO gyms(id,gym_id,name,created_at,updated_at) VALUES('gym','gym','Upgrade',1,1);
		INSERT INTO users(id,gym_id,email,password_hash,full_name,role,active,created_at,updated_at)
		  VALUES('owner','gym','owner@upgrade.test','hash','Owner','owner',1,1,1);
		INSERT INTO payments(id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		  payment_method,concept,balance_pending,payment_date,operator_id,cash_destination)
		VALUES('payment','gym',7,1,2,'P-1',12345,12345,'cash','other',0,'2026-09-06','owner','gym_fund');
		INSERT INTO sync_queue(id,entity_type,entity_id,operation,payload,client_version,enqueued_at,retry_count)
		VALUES('queued','payments','payment','upsert',
		  '{"id":"payment","gym_id":"gym","version":7,"amount":123.45,"cash_destination":"gym_fund","cash_drawer_id":null}',7,3,2);
	`)
	if err != nil {
		t.Fatal(err)
	}
	var originalPayload string
	if err := db.Get(&originalPayload, `SELECT payload FROM sync_queue WHERE id='queued'`); err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		if err := ApplySQLiteMigrations(db, os.DirFS("../../db_migrations"), "sqlite"); err != nil {
			t.Fatalf("upgrade/restart %d: %v", restart, err)
		}
		var paid, queued, migrations int
		if err := db.Get(&paid, `SELECT COUNT(*) FROM payments WHERE id='payment' AND amount=12345
		  AND recognized_amount=12345 AND version=7 AND cash_destination='gym_fund' AND cash_drawer_id IS NULL`); err != nil {
			t.Fatal(err)
		}
		if err := db.Get(&queued, `SELECT COUNT(*) FROM sync_queue WHERE id='queued' AND payload=?
		  AND client_version=7 AND enqueued_at=3 AND retry_count=2 AND synced_at IS NULL`, originalPayload); err != nil {
			t.Fatal(err)
		}
		if err := db.Get(&migrations, `SELECT COUNT(*) FROM _migrations WHERE version IN (43,44)`); err != nil {
			t.Fatal(err)
		}
		if paid != 1 || queued != 1 || migrations != 2 {
			t.Fatalf("data changed or upgrade incomplete: payment=%d queued=%d migrations=%d", paid, queued, migrations)
		}
		var integrity string
		if err := db.Get(&integrity, `PRAGMA integrity_check`); err != nil || integrity != "ok" {
			t.Fatalf("integrity=%s err=%v", integrity, err)
		}
		var violations int
		if err := db.Get(&violations, `SELECT COUNT(*) FROM pragma_foreign_key_check`); err != nil || violations != 0 {
			t.Fatalf("foreign keys=%d err=%v", violations, err)
		}
	}
}

func TestApplySQLiteMigrations_DoesNotAcceptIncompatibleCashDestination(t *testing.T) {
	db := sqliteBeforeCashDestination(t)
	if _, err := db.Exec(`ALTER TABLE payments ADD COLUMN cash_destination TEXT`); err != nil {
		t.Fatal(err)
	}
	if err := ApplySQLiteMigrations(db, os.DirFS("../../db_migrations"), "sqlite"); err == nil {
		t.Fatal("incompatible column must not be silently marked as migrated")
	}
	var count int
	if err := db.Get(&count, `SELECT COUNT(*) FROM _migrations WHERE version IN (43,44)`); err != nil || count != 0 {
		t.Fatalf("failed reconciliation marked migrations: count=%d err=%v", count, err)
	}
}
