//go:build sidecar

package sync_test

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

// TestApplyPullChange_Gym_MissingCreatedAt_PreservesLocal — post-fix del
// full-sync del piloto: un UPDATE cuyo payload omite created_at (enqueues
// históricos) ya NO rompe NOT NULL — ApplyPullChange rellena created_at
// con el valor LOCAL verdadero (la fila ya existe) y el resto de la fila
// sí se actualiza.
func TestApplyPullChange_Gym_MissingCreatedAt_PreservesLocal(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)

	// Captura el created_at original para comprobar que se preservó.
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
	if row.Version != 3 || row.Name != "Gym Pull" {
		t.Errorf("update no aplicó: version=%d name=%q", row.Version, row.Name)
	}
	// created_at local NO fue pisado por NULL ni por un fallback inventado.
	if row.CreatedAt != originalCreatedAt {
		t.Errorf("created_at mutated: was %d, now %d", originalCreatedAt, row.CreatedAt)
	}
}

// TestApplyPullChange_MembershipType_MissingCreatedAt_InsertFallback — el
// crash exacto del piloto: sidecar FRESCO (full-sync) inserta un
// membership_types cuyo payload guardado en sync_entities nunca trajo
// created_at (enqueueMT pre-fix). El apply debe completar usando el
// updated_at ORIGINAL del payload como created_at — no NULL, no error.
func TestApplyPullChange_MembershipType_MissingCreatedAt_InsertFallback(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)

	mtID := uuid.New()
	wireUpdatedMs := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	payload, err := json.Marshal(map[string]any{
		// Espejo de enqueueMT pre-fix: sin created_at y sin enrollment_fee/
		// maintenance_fee (payload anterior a la feature de cuotas — el
		// DEFAULT 0 del esquema debe aplicar, cosa que el NULL explícito
		// del builder viejo derrotaba). maintenance_frequency viaja como ""
		// (así serializa enqueueMT un puntero nil) — el CHECK del esquema
		// exige NULL cuando no hay cuota; pin del espejo de
		// nullifyEmptyString en extractColumnValue.
		"id":                    mtID.String(),
		"gym_id":                gymID.String(),
		"version":               1,
		"name":                  "Normal",
		"price":                 450.0, // pesos en el wire; el apply convierte a centavos
		"duration_days":         30,
		"maintenance_frequency": "",
		"active":                true,
		"updated_at":            wireUpdatedMs,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	change := syncpkg.PullChange{
		EntityType:      "membership_types",
		EntityID:        mtID.String(),
		Version:         1,
		Payload:         payload,
		ServerUpdatedAt: time.Now().UTC(),
	}
	err = uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, change)
	})
	if err != nil {
		t.Fatalf("apply (full-sync insert): %v", err)
	}

	var row struct {
		CreatedAt     int64   `db:"created_at"`
		Price         int64   `db:"price"`
		EnrollmentFee int64   `db:"enrollment_fee"`
		Freq          *string `db:"maintenance_frequency"`
	}
	if err := db.Get(&row, `SELECT created_at, price, enrollment_fee, maintenance_frequency FROM membership_types WHERE id = ?`, mtID); err != nil {
		t.Fatalf("read membership_type: %v", err)
	}
	// Fallback = updated_at original del payload (no el server_updated_at,
	// que es posterior; no NULL).
	if row.CreatedAt != wireUpdatedMs {
		t.Errorf("created_at = %d, want fallback %d", row.CreatedAt, wireUpdatedMs)
	}
	if row.Price != 45000 {
		t.Errorf("price = %d centavos, want 45000", row.Price)
	}
	// Llave ausente → columna omitida → DEFAULT 0 del esquema.
	if row.EnrollmentFee != 0 {
		t.Errorf("enrollment_fee = %d, want DEFAULT 0", row.EnrollmentFee)
	}
	// "" del wire → NULL (espejo de nullifyEmptyString); el CHECK del
	// esquema lo exige cuando maintenance_fee = 0.
	if row.Freq != nil {
		t.Errorf("maintenance_frequency = %q, want NULL", *row.Freq)
	}
}

// TestApplyPullChange_Gym_ISOTimestampString_CoercedToMs — pin del bug del
// recibo fantasma (jul-2026): enqueueGym pre-fix emitía setup_completed_at
// como *time.Time crudo → string RFC3339 en el payload guardado en
// sync_entities. El apply lo escribía TAL CUAL en la columna INTEGER
// (SQLite dynamic typing lo acepta sin ruido) y todo Scan posterior del gym
// en esa máquina moría con "converting string to int64" — EnqueueReceipt
// fallaba y el recibo de WhatsApp nunca se encolaba. El apply ahora coerce
// strings RFC3339 de columnas *_at a epoch-ms (espejo de coerceTimestamp
// del projector).
func TestApplyPullChange_Gym_ISOTimestampString_CoercedToMs(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)

	var pl map[string]any
	if err := json.Unmarshal(realisticGymPullPayload(t, gymID, 3, false), &pl); err != nil {
		t.Fatalf("unmarshal base payload: %v", err)
	}
	pl["setup_completed_at"] = "2026-06-28T19:14:04.453Z"
	payload, err := json.Marshal(pl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	change := syncpkg.PullChange{
		EntityType:      "gyms",
		EntityID:        gymID.String(),
		Version:         3,
		Payload:         payload,
		ServerUpdatedAt: time.Now().UTC(),
	}
	err = uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncpkg.ApplyPullChange(context.Background(), tx, change)
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// El Scan a int64 ES el pin: con el string crudo en la columna, este
	// Get reventaba igual que gymRepo.GetByID en producción.
	var setupMs int64
	if err := db.Get(&setupMs, `SELECT setup_completed_at FROM gyms WHERE id = ?`, gymID); err != nil {
		t.Fatalf("read setup_completed_at as int64: %v", err)
	}
	want := time.Date(2026, 6, 28, 19, 14, 4, 453_000_000, time.UTC).UnixMilli()
	if setupMs != want {
		t.Errorf("setup_completed_at = %d, want %d (epoch-ms del RFC3339)", setupMs, want)
	}
}

// TestMigration027_HealsPoisonedSetupCompletedAt — la migración de
// reparación debe convertir un setup_completed_at TEXT RFC3339 (escrito
// por el apply pre-fix) al epoch-ms exacto, in-place y de forma
// idempotente. Se envenena la fila a mano con el string EXACTO observado
// en producción y se re-aplica el archivo 027 (que ya corrió una vez al
// crear la DB del test — la re-aplicación también pin-ea la idempotencia).
func TestMigration027_HealsPoisonedSetupCompletedAt(t *testing.T) {
	gymID := uuid.New()
	db, _ := freshSidecarDBWithGym(t, gymID)

	if _, err := db.Exec(
		`UPDATE gyms SET setup_completed_at = '2026-06-28T19:14:04.453Z' WHERE id = ?`, gymID,
	); err != nil {
		t.Fatalf("poison row: %v", err)
	}

	sqlBytes, err := os.ReadFile("../../../db_migrations/sqlite/027_repair_gym_setup_completed_at.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := db.Exec(string(sqlBytes)); err != nil {
		t.Fatalf("apply migration 027: %v", err)
	}

	var ms int64
	if err := db.Get(&ms, `SELECT setup_completed_at FROM gyms WHERE id = ?`, gymID); err != nil {
		t.Fatalf("read healed row as int64: %v", err)
	}
	want := time.Date(2026, 6, 28, 19, 14, 4, 453_000_000, time.UTC).UnixMilli()
	if ms != want {
		t.Errorf("setup_completed_at = %d, want %d", ms, want)
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
