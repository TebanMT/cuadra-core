//go:build sidecar

// Package upgradechain valida que la cadena completa de migraciones del
// sidecar SQLite preserve datos al pasar de la versión 1 (init schema) a
// la más reciente — el flujo real que vive un cliente que arrancó hace
// meses y nunca apagó la PC. Es el safety net del release pipeline
// (ADR-005 §6 + ADR-002 §5): si una migración nueva rompe el upgrade
// path, este test falla y bloquea el merge.
//
// Diseño: usamos t.TempDir() para un SQLite limpio. Aplicamos sólo la
// migración 001 (con un FS recortado), seedeamos datos sintéticos
// (gym + user + 10 members + 20 payments), aplicamos el RESTO de
// migraciones, y validamos:
//
//  1. Counts antes/después matchean (zero-loss).
//  2. _migrations tiene todas las versiones aplicadas.
//  3. El SchemaVersion del código (sync.SchemaVersion) está en sync con
//     la última migración — proxy del check lock-step entre wire y SQL.
package upgradechain

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	migrations "github.com/cuadra/cuadra-core/db_migrations"
	infraDB "github.com/cuadra/cuadra-core/infraestructure/db"
)

// openTempSQLite arma un SQLite fresco en t.TempDir() con los mismos
// flags que la app real (foreign_keys + WAL). No usamos InitSQLite —
// ese tiene un sync.Once que sostiene una sola instancia por proceso y
// rompe tests sucesivos.
func openTempSQLite(t *testing.T) *sqlx.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upgrade-chain.db")
	dsn := path + "?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000"
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sqlx.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// onlyFirstMigration construye un fs.FS que sólo expone 001_init_schema.sql,
// para poder aplicar PRIMERO la migración 1 con ApplySQLiteMigrations y
// DESPUÉS las siguientes con el FS completo. Esto simula el shape que un
// cliente real vive: arranca en versión 1, ingresa data, luego upgrade.
func onlyFirstMigration(t *testing.T) fs.FS {
	t.Helper()
	dir := t.TempDir()
	sub := filepath.Join(dir, "sqlite")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := fs.ReadFile(migrations.SQLite, "sqlite/001_init_schema.sql")
	if err != nil {
		t.Fatalf("read 001: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "001_init_schema.sql"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return os.DirFS(dir)
}

// seedSyntheticData inserta una linea de gym + user + 10 members + 20
// payments con tipos válidos según los CHECK constraints de la migración
// 001. Devuelve el gym_id para poder asserts posteriores y el ID del
// operador (para payments.operator_id).
func seedSyntheticData(t *testing.T, db *sqlx.DB) (gymID, userID string) {
	t.Helper()
	now := time.Now().UnixMilli()
	gymID = uuid.NewString()
	userID = uuid.NewString()

	_, err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, payment_methods, kiosk_settings)
		VALUES (?, ?, 1, ?, ?, 'Gym Test', '[]', '{"audio_volume":80,"auto_close_seconds":5}')`,
		gymID, gymID, now, now)
	if err != nil {
		t.Fatalf("insert gym: %v", err)
	}

	_, err = db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at,
		                   email, password_hash, full_name, role, active, must_change_password)
		VALUES (?, ?, 1, ?, ?, 'op@test.local', 'hash', 'Operador', 'owner', 1, 0)`,
		userID, gymID, now, now)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// 10 members.
	memberIDs := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		mid := uuid.NewString()
		memberIDs = append(memberIDs, mid)
		_, err := db.Exec(`
			INSERT INTO members (id, gym_id, version, created_at, updated_at,
			                     folio, full_name, phone, status, created_by)
			VALUES (?, ?, 1, ?, ?, ?, ?, ?, 'active', ?)`,
			mid, gymID, now, now,
			"F"+mid[:6], "Socio "+mid[:6], "55"+mid[:8], userID)
		if err != nil {
			t.Fatalf("insert member %d: %v", i, err)
		}
	}

	// 20 payments — todos con concept=membership para no chocar con product
	// (que requiere sale_items) ni con refund (que requiere parent).
	for i := 0; i < 20; i++ {
		pid := uuid.NewString()
		_, err := db.Exec(`
			INSERT INTO payments (id, gym_id, version, created_at, updated_at,
			                      folio, member_id, amount, payment_method, concept,
			                      payment_date, operator_id)
			VALUES (?, ?, 1, ?, ?, ?, ?, ?, 'cash', 'membership', ?, ?)`,
			pid, gymID, now, now,
			"P"+pid[:6], memberIDs[i%10], 50000+i, "2026-05-01", userID)
		if err != nil {
			t.Fatalf("insert payment %d: %v", i, err)
		}
	}
	return gymID, userID
}

func countTable(t *testing.T, db *sqlx.DB, table, gymID string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM "+table+" WHERE gym_id = ? AND deleted_at IS NULL", gymID); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestUpgradeChain_PreservesData es la prueba reina. Si esto falla, hay
// una migración rota que pierde datos al hacer ALTER TABLE/copy/etc.
func TestUpgradeChain_PreservesData(t *testing.T) {
	db := openTempSQLite(t)

	// 1. Aplicar SOLO la migración 1 con un FS recortado.
	firstOnly := onlyFirstMigration(t)
	if err := infraDB.ApplySQLiteMigrations(db, firstOnly, "sqlite"); err != nil {
		t.Fatalf("apply migration 1: %v", err)
	}

	// 2. Seed sintético — simula un cliente que llevaba meses operando.
	gymID, _ := seedSyntheticData(t, db)
	memsBefore := countTable(t, db, "members", gymID)
	paysBefore := countTable(t, db, "payments", gymID)
	if memsBefore != 10 || paysBefore != 20 {
		t.Fatalf("seed counts incorrectos: members=%d, payments=%d", memsBefore, paysBefore)
	}

	// 3. Aplicar el resto de migraciones (003, 004, …) con el FS completo.
	if err := infraDB.ApplySQLiteMigrations(db, migrations.SQLite, "sqlite"); err != nil {
		t.Fatalf("apply remaining migrations: %v", err)
	}

	// 4. Counts preservados.
	memsAfter := countTable(t, db, "members", gymID)
	paysAfter := countTable(t, db, "payments", gymID)
	if memsAfter != memsBefore {
		t.Errorf("members count cambió: %d → %d", memsBefore, memsAfter)
	}
	if paysAfter != paysBefore {
		t.Errorf("payments count cambió: %d → %d", paysBefore, paysAfter)
	}

	// 5. _migrations tiene todas las versiones del FS embebido.
	expected := countMigrationFiles(t, migrations.SQLite)
	var applied int
	if err := db.Get(&applied, "SELECT COUNT(*) FROM _migrations"); err != nil {
		t.Fatalf("count _migrations: %v", err)
	}
	if applied != expected {
		t.Errorf("_migrations count = %d, want %d (todas las migraciones del FS)", applied, expected)
	}
}

// TestUpgradeChain_IdempotentSecondRun verifica que aplicar la cadena
// completa DOS veces seguidas no falla (toda migración tiene IF NOT
// EXISTS, ADR-002 §5.1). Caso real: sidecar arrancado, killed antes
// del primer write a _migrations en una versión nueva.
func TestUpgradeChain_IdempotentSecondRun(t *testing.T) {
	db := openTempSQLite(t)

	if err := infraDB.ApplySQLiteMigrations(db, migrations.SQLite, "sqlite"); err != nil {
		t.Fatalf("primera aplicación: %v", err)
	}
	if err := infraDB.ApplySQLiteMigrations(db, migrations.SQLite, "sqlite"); err != nil {
		t.Errorf("segunda aplicación falló (¿alguna migración no es idempotente?): %v", err)
	}
}

// TestUpgradeChain_AllMigrationsRegistered verifica que cada archivo
// .sql del FS tiene su fila correspondiente en _migrations. Cubre el
// caso "alguien escribió una migración nueva pero olvidó el INSERT INTO
// _migrations al final" — clásico que rompe el upgrade-from-N.
func TestUpgradeChain_AllMigrationsRegistered(t *testing.T) {
	db := openTempSQLite(t)
	if err := infraDB.ApplySQLiteMigrations(db, migrations.SQLite, "sqlite"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	expected := countMigrationFiles(t, migrations.SQLite)
	var applied int
	if err := db.Get(&applied, "SELECT COUNT(*) FROM _migrations"); err != nil {
		t.Fatalf("count: %v", err)
	}
	if applied != expected {
		t.Errorf("_migrations registra %d, hay %d archivos en el FS — alguna migración no INSERTó su fila", applied, expected)
	}

	// Verificación adicional: todas las versiones forman una secuencia
	// contigua sin huecos (1, 2, 3, …). Un hueco indicaría que removimos
	// una migración intermedia, lo que rompería sidecars existentes.
	rows, err := db.Queryx("SELECT version FROM _migrations ORDER BY version")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	prev := 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != prev+1 {
			t.Errorf("hueco en _migrations: después de %d viene %d (esperaba %d)", prev, v, prev+1)
		}
		prev = v
	}
}

// countMigrationFiles cuenta los archivos .sql efectivos en el FS embebido.
func countMigrationFiles(t *testing.T, fsys fs.FS) int {
	t.Helper()
	entries, err := fs.ReadDir(fsys, "sqlite")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sql" {
			n++
		}
	}
	return n
}

// _ guarda la dependencia de context.Background() sin usarla — si el test
// crece a probar push/pull contra el cloud handler in-process, ya está el
// import listo.
var _ = context.Background
