//go:build server && integration

package infraestructure_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/cuadra/cuadra-core/src/application/releases"
	"github.com/cuadra/cuadra-core/src/application/releases/infraestructure"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func openTestDB(t *testing.T) (*gorm.DB, sharedDomain.UnitOfWork) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/cuadra?sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("integration test skipped — Postgres unreachable: %v", err)
	}
	return db, sharedDomain.NewPostgresUnitOfWork(db)
}

// seedRelease inserta una fila directa para que LatestForTarget tenga algo
// que devolver. Devuelve la versión usada para que el caller la chequee.
func seedRelease(t *testing.T, db *gorm.DB, version, target, channel string, pubDate time.Time, rollout int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := db.Exec(`
		INSERT INTO releases (id, version, target_platform, channel, url,
		                      signature_ed25519, pub_date, notes,
		                      rollout_percent, force_immediate)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, FALSE)`,
		id, version, target, channel,
		"https://releases.entinta.app/test/"+version+".exe",
		"sig-"+version, pubDate, "test", rollout).Error
	if err != nil {
		t.Fatalf("seed release: %v", err)
	}
	t.Cleanup(func() { _ = db.Exec(`DELETE FROM releases WHERE id = ?`, id).Error })
	return id
}

func TestPostgres_LatestForTarget_NingunaFila(t *testing.T) {
	db, uow := openTestDB(t)
	store := infraestructure.NewPostgresStore()
	// Limpiamos cualquier residuo del target sintético antes de medir.
	target := "test-empty-" + uuid.NewString()
	tx, _ := uow.Query(context.Background())
	got, err := store.LatestForTarget(tx, target, "stable")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("esperaba nil, got %+v", got)
	}
	_ = db // mantenemos handle por si limpieza fuera necesaria
}

func TestPostgres_LatestForTarget_DevuelveMasReciente(t *testing.T) {
	db, uow := openTestDB(t)
	store := infraestructure.NewPostgresStore()
	// target único por test para no chocar con datos preexistentes.
	target := "test-" + uuid.NewString()
	old := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Microsecond)
	newer := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Microsecond)
	seedRelease(t, db, "1.0.0", target, "stable", old, 100)
	seedRelease(t, db, "1.1.0", target, "stable", newer, 100)

	tx, _ := uow.Query(context.Background())
	rel, err := store.LatestForTarget(tx, target, "stable")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rel == nil || rel.Version != "1.1.0" {
		t.Errorf("esperaba 1.1.0, got %+v", rel)
	}
}

func TestPostgres_LatestForTarget_FiltraCanal(t *testing.T) {
	db, uow := openTestDB(t)
	store := infraestructure.NewPostgresStore()
	target := "test-" + uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedRelease(t, db, "1.0.0-beta", target, "beta", now, 100)
	seedRelease(t, db, "0.9.0", target, "stable", now.Add(-72*time.Hour), 100)

	tx, _ := uow.Query(context.Background())

	stable, err := store.LatestForTarget(tx, target, "stable")
	if err != nil {
		t.Fatalf("err stable: %v", err)
	}
	if stable == nil || stable.Version != "0.9.0" {
		t.Errorf("stable: got %+v", stable)
	}

	beta, err := store.LatestForTarget(tx, target, "beta")
	if err != nil {
		t.Fatalf("err beta: %v", err)
	}
	if beta == nil || beta.Version != "1.0.0-beta" {
		t.Errorf("beta: got %+v", beta)
	}
	_ = db
}

func TestPostgres_Insert_RoundTrip(t *testing.T) {
	_, uow := openTestDB(t)
	store := infraestructure.NewPostgresStore()
	target := "test-" + uuid.NewString()
	rel := releases.Release{
		ID:               uuid.New(),
		Version:          "2.0.0",
		TargetPlatform:   target,
		Channel:          "stable",
		URL:              "https://releases.entinta.app/win/x.exe",
		SignatureEd25519: "deadbeef",
		PubDate:          time.Now().UTC().Truncate(time.Microsecond),
		Notes:            "test",
		RolloutPercent:   5,
	}
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return store.Insert(tx, rel)
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		db, _ := openTestDB(t)
		_ = db.Exec(`DELETE FROM releases WHERE id = ?`, rel.ID).Error
	})

	tx, _ := uow.Query(context.Background())
	got, err := store.LatestForTarget(tx, target, "stable")
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
	if got == nil || got.Version != "2.0.0" || got.RolloutPercent != 5 {
		t.Errorf("read-back: got %+v", got)
	}
}
