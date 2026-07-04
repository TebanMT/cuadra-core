//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// freshSidecarDBWithGym devuelve un sqlite con TODAS las migraciones
// aplicadas y un row en `gyms` bootstrap-eado al estilo "mirror gym" del
// auth_controller_sidecar (first-login): sólo bind-ea los campos que el
// cloud envió, dejando que los defaults del esquema poblen el resto
// (kiosk_settings, subscription_*, payment_methods).
//
// Es el escenario real del bug que reportó el piloto: la sidecar SÍ tiene
// un row de gym local (vino del first-login handshake), y el sync agent
// pull-ea un payload del cloud cuya versión es mayor → ApplyPullChange
// ejecuta INSERT ... ON CONFLICT DO UPDATE SET col = excluded.col para
// CADA columna registrada, incluso si el payload no la trae. Sin la
// columna en el payload, excluded.col es NULL y la UPDATE rompe NOT NULL.
func freshSidecarDBWithGym(t *testing.T, gymID uuid.UUID) (*sqlx.DB, sharedDomain.UnitOfWork) {
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
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(migDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("apply %s: %v", e.Name(), err)
		}
	}
	uow := sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue())

	now := time.Now().UTC().UnixMilli()
	// Bootstrap igual que auth_controller_sidecar.mirror gym: id+gym_id+
	// version+created_at+updated_at+name. Schema defaults llenan
	// kiosk_settings, subscription_*, payment_methods, country, timezone.
	_, err = db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name)
		VALUES (?, ?, 1, ?, ?, 'Gym Pilot')`,
		gymID, gymID, now, now)
	if err != nil {
		t.Fatalf("bootstrap gym: %v", err)
	}
	return db, uow
}

// realisticGymPullPayload construye el payload que un sidecar emite hoy
// vía enqueueGym(gym_sqlite.go), MÁS los campos que el cloud necesitaría
// inyectar para que un pull complete sin romper otras NOT NULL columns.
// Si omitCreatedAt=true, elimina la clave created_at — espeja el bug
// pre-fix exactamente.
//
// NOTA: este test deliberadamente pone subscription_plan, subscription_status
// y kiosk_settings en el payload aunque enqueueGym hoy NO los emite. Es
// para aislar el bug de created_at del test. En producción, un sidecar B
// pull-eando un payload real de un sidecar A todavía fallaría con
// `NOT NULL constraint failed: gyms.subscription_status` o
// `gyms.kiosk_settings` después de fixear created_at. Ver agent_apply_test
// más abajo y el reporte del Bug 1 en la conversación.
func realisticGymPullPayload(t *testing.T, gymID uuid.UUID, version int, omitCreatedAt bool) []byte {
	t.Helper()
	createdMs := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC).UnixMilli()
	updatedMs := time.Now().UTC().UnixMilli()
	pl := map[string]any{
		"id":                  gymID.String(),
		"gym_id":              gymID.String(),
		"version":             version,
		"name":                "Gym Pull",
		"country":             "MX",
		"timezone":            "America/Mexico_City",
		"payment_methods":     []string{"cash"},
		"charge_settings":     map[string]any{},
		"updated_at":          updatedMs,
		"subscription_plan":   "trial",
		"subscription_status": "active",
		"kiosk_settings":      map[string]any{"audio_volume": 80, "auto_close_seconds": 5},
	}
	if !omitCreatedAt {
		pl["created_at"] = createdMs
	}
	b, err := json.Marshal(pl)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// TestApplyPullChange_Gym_WithCreatedAt_Succeeds — post-fix: el payload
// incluye created_at → ApplyPullChange aplica la UPDATE sin romper NOT NULL.
func TestApplyPullChange_Gym_WithCreatedAt_Succeeds(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)

	payload := realisticGymPullPayload(t, gymID, 3, false)
	change := syncpkg.PullChange{
		EntityType:      "gyms",
		EntityID:        gymID.String(),
		Version:         3,
		Payload:         payload,
		ServerUpdatedAt: time.Now().UTC(),
	}
	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, change)
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	var row struct {
		CreatedAt int64  `db:"created_at"`
		Name      string `db:"name"`
		Version   int    `db:"version"`
	}
	if err := db.Get(&row, `SELECT created_at, name, version FROM gyms WHERE id = ?`, gymID); err != nil {
		t.Fatalf("read gym: %v", err)
	}
	if row.Name != "Gym Pull" {
		t.Errorf("name = %q, want Gym Pull", row.Name)
	}
	if row.Version != 3 {
		t.Errorf("version = %d, want 3", row.Version)
	}
	// Sanity: created_at quedó con el valor del payload, no NULL.
	wantCreatedMs := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC).UnixMilli()
	if row.CreatedAt != wantCreatedMs {
		t.Errorf("created_at = %d, want %d", row.CreatedAt, wantCreatedMs)
	}
}

// TestApplyPullChange_Gym_MissingCreatedAt_FailsCleanly — pre-fix: el
// payload omite created_at → ApplyPullChange devuelve el error de NOT NULL
// (envuelto en el error de la transacción), sin panic. Pin-ea el bug
// observado en el piloto y previene regresiones si enqueueGym vuelve a
// omitir la columna.
func TestApplyPullChange_Gym_MissingCreatedAt_FailsCleanly(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)

	// Captura el created_at original para comprobar que no se sobreescribió.
	var originalCreatedAt int64
	if err := db.Get(&originalCreatedAt, `SELECT created_at FROM gyms WHERE id = ?`, gymID); err != nil {
		t.Fatalf("read original: %v", err)
	}

	payload := realisticGymPullPayload(t, gymID, 3, true) // omit created_at
	change := syncpkg.PullChange{
		EntityType:      "gyms",
		EntityID:        gymID.String(),
		Version:         3,
		Payload:         payload,
		ServerUpdatedAt: time.Now().UTC(),
	}
	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, change)
	})
	if err == nil {
		t.Fatalf("expected NOT NULL error, got nil")
	}
	if !strings.Contains(err.Error(), "NOT NULL") || !strings.Contains(err.Error(), "created_at") {
		t.Errorf("expected NOT NULL/created_at in error, got: %v", err)
	}

	// Verifica que la transacción rollback-eó: el created_at sigue siendo
	// el original del bootstrap, no NULL.
	var after int64
	if err := db.Get(&after, `SELECT created_at FROM gyms WHERE id = ?`, gymID); err != nil {
		t.Fatalf("read after: %v", err)
	}
	if after != originalCreatedAt {
		t.Errorf("created_at mutated despite rollback: was %d, now %d", originalCreatedAt, after)
	}
}

// subscriptionPullPayload — el payload que el cloud emite en un pull tras un
// touch de suscripción: el payload guardado del último push, con los campos
// billing vivos inyectados por gymCanonicalAugmentExpr encima.
func subscriptionPullPayload(t *testing.T, gymID uuid.UUID, version int, plan, status string) []byte {
	t.Helper()
	var pl map[string]any
	if err := json.Unmarshal(realisticGymPullPayload(t, gymID, version, false), &pl); err != nil {
		t.Fatalf("unmarshal base payload: %v", err)
	}
	pl["subscription_plan"] = plan
	pl["subscription_status"] = status
	b, err := json.Marshal(pl)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// TestApplyPullChange_Gym_TouchSuscripcion_LWWPorVersion — escenario completo
// del gap de propagación de suscripciones (caso 2, memoria
// project_sync_propagation_gap): tras un push exitoso, el sidecar que pusheó
// queda con local.version == sync_entities.version (stampLocalSynced). Un
// touch cloud-side timestamp-only re-manda la fila con esa MISMA versión y
// este sidecar — el caso común de UN solo desktop — la descarta por LWW.
//
//  1. Pin del hoyo: PullChange con version == local se descarta (sin error,
//     idempotente). Documenta por qué TouchGym debe bumpear la versión del
//     journal, no sólo server_updated_at.
//  2. Fix: PullChange con version local+1 (touch con bump) aplica y el
//     subscription_plan/status vivos aterrizan en el row local.
func TestApplyPullChange_Gym_TouchSuscripcion_LWWPorVersion(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	// Estado post-push: local.version == journal.version == 3. Los defaults
	// del esquema dejan subscription_plan='trial' — el valor stale que el
	// touch debe reemplazar.
	if _, err := db.Exec(`UPDATE gyms SET version = 3 WHERE id = ?`, gymID); err != nil {
		t.Fatalf("set local version: %v", err)
	}

	apply := func(version int) error {
		change := syncpkg.PullChange{
			EntityType:      "gyms",
			EntityID:        gymID.String(),
			Version:         version,
			Payload:         subscriptionPullPayload(t, gymID, version, "standard_monthly", "active"),
			ServerUpdatedAt: time.Now().UTC(),
		}
		return uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
			return syncpkg.ApplyPullChange(context.Background(), tx, change)
		})
	}

	readGym := func() (plan, status string, version int) {
		var row struct {
			Plan    string `db:"subscription_plan"`
			Status  string `db:"subscription_status"`
			Version int    `db:"version"`
		}
		if err := db.Get(&row, `
			SELECT subscription_plan, subscription_status, version
			  FROM gyms WHERE id = ?`, gymID); err != nil {
			t.Fatalf("read gym: %v", err)
		}
		return row.Plan, row.Status, row.Version
	}

	// 1. Touch viejo (timestamp-only, versión empatada): descartado.
	if err := apply(3); err != nil {
		t.Fatalf("apply version empatada: %v", err)
	}
	plan, _, version := readGym()
	if plan != "trial" || version != 3 {
		t.Fatalf("el pull con version == local NO debía aplicar: plan=%q version=%d", plan, version)
	}

	// 2. Touch con bump de versión: aplica el estado vivo.
	if err := apply(4); err != nil {
		t.Fatalf("apply version bumpeada: %v", err)
	}
	plan, status, version := readGym()
	if plan != "standard_monthly" || status != "active" {
		t.Errorf("la suscripción no aterrizó: plan=%q status=%q", plan, status)
	}
	if version != 4 {
		t.Errorf("version local = %d, want 4", version)
	}
}
