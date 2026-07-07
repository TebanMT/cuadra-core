//go:build sidecar

package sync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Estos tests pinean el self-heal del pull (dogfood 6-jul-2026): una sola
// fila que no se puede aplicar NO debe wedge-ear todo el sync para
// siempre. Tras `quarantineThreshold` intentos, el pull la salta, aplica
// el resto y avanza el cursor — con constancia (no silencioso) y
// re-intento cuando el cloud sube su version.
//
// Se prueba la maquinaria (probePoison, recordQuarantineAttempts,
// quarantineAndRetry) contra un SQLite real con el esquema completo.

func quarantineTestAgent(t *testing.T, gymID uuid.UUID) (*Agent, *sqlx.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlx.Open("sqlite3", filepath.Join(dir, "test.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	migDir := "../../../db_migrations/sqlite"
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(migDir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		if _, eerr := db.Exec(string(b)); eerr != nil {
			t.Fatalf("apply %s: %v", e.Name(), eerr)
		}
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(
		`INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name)
		 VALUES (?, ?, 1, ?, ?, 'Gym Pilot')`, gymID, gymID, now, now); err != nil {
		t.Fatalf("gym: %v", err)
	}
	uow := sharedDomain.NewSQLiteUnitOfWork(db, NewSqliteQueue())
	a := NewAgent(AgentConfig{BaseURL: "http://x"}, db, uow)
	return a, db
}

// productChange arma un PullChange VÁLIDO de products (aplica sin drama).
func productChange(gymID uuid.UUID, name string, version int, updatedMs int64) PullChange {
	id := uuid.New()
	return PullChange{
		EntityType: "products",
		EntityID:   id.String(),
		Version:    version,
		Payload: qJSON(map[string]any{
			"id": id.String(), "gym_id": gymID.String(), "version": version,
			"created_at": updatedMs, "updated_at": updatedMs,
			"name": name, "price": 1000, "stock": 5, "active": true,
		}),
		ServerUpdatedAt: time.UnixMilli(updatedMs).UTC(),
	}
}

func qJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// TestQuarantine_SkipsPoisonAfterThreshold — el corazón del self-heal.
func TestQuarantine_SkipsPoisonAfterThreshold(t *testing.T) {
	gymID := uuid.New()
	a, db := quarantineTestAgent(t, gymID)
	ctx := context.Background()
	base := time.Now().UTC().UnixMilli()

	good1 := productChange(gymID, "Agua", 1, base+1)
	bad := poisonChangeCheckViolation(gymID, 1, base+2)
	good2 := productChange(gymID, "Barra", 1, base+3)
	batch := []PullChange{good1, bad, good2}

	advance := func(tx sharedDomain.Transaction) error {
		return SetLastPulledAt(ctx, tx, batch[len(batch)-1].ServerUpdatedAt)
	}

	// Bajo el umbral: NO salta todavía (handled=false).
	for i := 1; i < quarantineThreshold; i++ {
		handled, err := a.quarantineAndRetry(ctx, batch, advance)
		if err != nil {
			t.Fatalf("intento %d: %v", i, err)
		}
		if handled {
			t.Fatalf("intento %d: saltó ANTES del umbral (%d)", i, quarantineThreshold)
		}
		assertProductCount(t, db, 0) // nada aplicado aún
	}

	// Al alcanzar el umbral: salta el veneno, aplica los 2 buenos, avanza.
	handled, err := a.quarantineAndRetry(ctx, batch, advance)
	if err != nil {
		t.Fatalf("intento umbral: %v", err)
	}
	if !handled {
		t.Fatal("no saltó el veneno al cruzar el umbral")
	}
	assertProductCount(t, db, 2) // los 2 buenos aterrizaron

	// El veneno quedó registrado (visible, no silencioso).
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM sync_quarantine WHERE attempts >= ?`, quarantineThreshold); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("sync_quarantine cuenta %d, want 1", n)
	}

	// El cursor avanzó hasta el final de la página (dejando el veneno atrás).
	var pulled int64
	if err := db.Get(&pulled, `SELECT CAST(value AS INTEGER) FROM sync_state WHERE key = 'last_pulled_at_ms'`); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if pulled != good2.ServerUpdatedAt.UnixMilli() {
		t.Errorf("last_pulled_at = %d, want %d (fin de página)", pulled, good2.ServerUpdatedAt.UnixMilli())
	}
}

// TestQuarantine_VersionBumpResetsAttempts — auto-cura: si el cloud sube la
// version (dato corregido), el contador se resetea y se reintenta.
func TestQuarantine_VersionBumpResetsAttempts(t *testing.T) {
	gymID := uuid.New()
	a, db := quarantineTestAgent(t, gymID)
	ctx := context.Background()
	base := time.Now().UTC().UnixMilli()

	bad := poisonChangeCheckViolation(gymID, 1, base+1)
	batch := []PullChange{bad}
	advance := func(tx sharedDomain.Transaction) error { return nil }

	// Falla hasta el umbral.
	for i := 0; i < quarantineThreshold; i++ {
		_, _ = a.quarantineAndRetry(ctx, batch, advance)
	}
	var attempts int
	_ = db.Get(&attempts, `SELECT attempts FROM sync_quarantine WHERE entity_id = ?`, bad.EntityID)
	if attempts < quarantineThreshold {
		t.Fatalf("attempts=%d, want >= %d", attempts, quarantineThreshold)
	}

	// El cloud sube la version (MISMA fila, version 5) → reset a 1.
	bumped := PullChange{
		EntityType: "products",
		EntityID:   bad.EntityID,
		Version:    5,
		Payload: qJSON(map[string]any{
			"id": bad.EntityID, "gym_id": gymID.String(), "version": 5,
			"created_at": base + 10, "updated_at": base + 10,
			"name": "Veneno", "price": -100, "stock": 5, "active": true,
		}),
		ServerUpdatedAt: time.UnixMilli(base + 10).UTC(),
	}
	_, _ = a.quarantineAndRetry(ctx, []PullChange{bumped}, advance)

	_ = db.Get(&attempts, `SELECT attempts FROM sync_quarantine WHERE entity_id = ?`, bad.EntityID)
	if attempts != 1 {
		t.Errorf("tras bump de version, attempts=%d, want 1 (reset para reintento fresco)", attempts)
	}
}

// TestQuarantine_HealClearsRow — cuando una fila antes venenosa aplica bien
// (se curó), su registro de cuarentena se borra y el conteo baja.
func TestQuarantine_HealClearsRow(t *testing.T) {
	gymID := uuid.New()
	a, db := quarantineTestAgent(t, gymID)
	ctx := context.Background()
	base := time.Now().UTC().UnixMilli()

	healed := productChange(gymID, "Toalla", 2, base+1)
	// Sembramos una fila de cuarentena para esa entidad (como si hubiera
	// fallado antes con version 1).
	if _, err := db.Exec(`
		INSERT INTO sync_quarantine (entity_type, entity_id, version, attempts, last_error, first_seen_at, last_seen_at)
		VALUES ('products', ?, 1, ?, 'boom', ?, ?)`,
		healed.EntityID, quarantineThreshold, base, base); err != nil {
		t.Fatalf("seed quarantine: %v", err)
	}
	a.state.QuarantinedCount = 1

	// Aplica limpio y luego limpia la cuarentena de las filas aplicadas.
	if err := ApplyPullPage(ctx, a.uow, []PullChange{healed}, func(tx sharedDomain.Transaction) error { return nil }); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		return clearQuarantineForApplied(ctx, tx.(*sharedDomain.SqlxTransaction), []PullChange{healed})
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}

	var n int
	_ = db.Get(&n, `SELECT COUNT(*) FROM sync_quarantine`)
	if n != 0 {
		t.Errorf("cuarentena no se limpió tras curarse: quedan %d", n)
	}
}

// --- helpers ---

// poisonChangeCheckViolation viola el CHECK de products.price (>= 0) con un
// precio negativo → error inmediato no-FK al INSERT (veneno determinista).
func poisonChangeCheckViolation(gymID uuid.UUID, version int, updatedMs int64) PullChange {
	id := uuid.New()
	return PullChange{
		EntityType: "products",
		EntityID:   id.String(),
		Version:    version,
		Payload: qJSON(map[string]any{
			"id": id.String(), "gym_id": gymID.String(), "version": version,
			"created_at": updatedMs, "updated_at": updatedMs,
			"name": "Veneno", "price": -100, "stock": 5, "active": true,
		}),
		ServerUpdatedAt: time.UnixMilli(updatedMs).UTC(),
	}
}

func assertProductCount(t *testing.T, db *sqlx.DB, want int) {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM products WHERE deleted_at IS NULL`); err != nil {
		t.Fatalf("count products: %v", err)
	}
	if n != want {
		t.Fatalf("products = %d, want %d", n, want)
	}
}
