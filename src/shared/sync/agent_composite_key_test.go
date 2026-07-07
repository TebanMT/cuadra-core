//go:build sidecar

package sync

import (
	"context"
	"database/sql"
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

// Regresión del bug del piloto (6-jul-2026): "push: no such column: id".
// owner_alert_configs es la ÚNICA tabla sincronizada sin columna `id` — su
// PK es (gym_id, alert_key) y el registry la declara con CompositeKey. Pero
// stampLocalSynced (write-back del push) y ApplyPullChange (aterrizaje del
// pull) hacían `WHERE id = ?` / `ON CONFLICT(id)` sin honrar CompositeKey,
// así que TODA fila de owner_alert_configs reventaba con "no such column:
// id". Como el write-back del batch corre en UNA transacción, esa fila
// atascada hacía rollback del batch ENTERO y envenenaba cada push
// siguiente — por eso el operador vio el error al crear una promo/socio
// (filas sanas), sin que fueran ellas la causa.

func compositeKeyTestDB(t *testing.T, gymID uuid.UUID) (*sqlx.DB, sharedDomain.UnitOfWork) {
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
		 VALUES (?, ?, 1, ?, ?, 'Gym Pilot')`,
		gymID, gymID, now, now,
	); err != nil {
		t.Fatalf("bootstrap gym: %v", err)
	}
	return db, sharedDomain.NewSQLiteUnitOfWork(db, NewSqliteQueue())
}

func alertPayload(gymID uuid.UUID, alertKey string, enabled bool, version int, updatedMs int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"gym_id":     gymID.String(),
		"alert_key":  alertKey,
		"enabled":    enabled,
		"version":    version,
		"updated_at": updatedMs,
	})
	return b
}

// TestStampLocalSynced_OwnerAlertConfigs — el write-back del push que
// produjo el error del operador. Antes: "no such column: id".
func TestStampLocalSynced_OwnerAlertConfigs(t *testing.T) {
	gymID := uuid.New()
	db, uow := compositeKeyTestDB(t, gymID)

	updatedMs := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(
		`INSERT INTO owner_alert_configs (gym_id, alert_key, enabled, version, updated_at, synced_at)
		 VALUES (?, 'owner_alert_low_stock', 1, 1, ?, NULL)`,
		gymID, updatedMs,
	); err != nil {
		t.Fatalf("seed alert config: %v", err)
	}

	// El entity_id de owner_alert_configs es un UUIDv5 derivado, NO está en
	// la tabla — pasamos uno arbitrario a propósito para probar que la fila
	// se ubica por el (gym_id, alert_key) del payload, no por entity_id.
	item := PushItem{
		QueueID:       uuid.New().String(),
		EntityType:    "owner_alert_configs",
		EntityID:      uuid.New().String(),
		Operation:     "upsert",
		ClientVersion: 1,
		Payload:       alertPayload(gymID, "owner_alert_low_stock", true, 1, updatedMs),
	}

	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		return stampLocalSynced(context.Background(), stx, item, 1, updatedMs)
	})
	if err != nil {
		t.Fatalf("stampLocalSynced: %v (regresión del bug 'no such column: id')", err)
	}

	var syncedAt sql.NullInt64
	if err := db.Get(&syncedAt,
		`SELECT synced_at FROM owner_alert_configs WHERE gym_id = ? AND alert_key = 'owner_alert_low_stock'`,
		gymID,
	); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !syncedAt.Valid {
		t.Error("synced_at sigue NULL — el stamp no ubicó la fila por su llave compuesta")
	}
}

// TestApplyPullChange_OwnerAlertConfigs — el pull también hacía WHERE id/
// ON CONFLICT(id). Verifica insert desde cloud, LWW skip por versión menor,
// y update por versión mayor.
func TestApplyPullChange_OwnerAlertConfigs(t *testing.T) {
	gymID := uuid.New()
	db, uow := compositeKeyTestDB(t, gymID)
	updatedMs := time.Now().UTC().UnixMilli()

	apply := func(version int, enabled bool) error {
		return uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
			return ApplyPullChange(context.Background(), tx, PullChange{
				EntityType:      "owner_alert_configs",
				EntityID:        uuid.New().String(), // irrelevante para tablas compuestas
				Version:         version,
				Payload:         alertPayload(gymID, "owner_alert_expired_batch", enabled, version, updatedMs),
				ServerUpdatedAt: time.Now().UTC(),
			})
		})
	}

	// INSERT desde el cloud (no había fila local).
	if err := apply(2, false); err != nil {
		t.Fatalf("apply v2: %v", err)
	}
	var row struct {
		Enabled int `db:"enabled"`
		Version int `db:"version"`
	}
	q := `SELECT enabled, version FROM owner_alert_configs WHERE gym_id = ? AND alert_key = 'owner_alert_expired_batch'`
	if err := db.Get(&row, q, gymID); err != nil {
		t.Fatalf("read after v2: %v", err)
	}
	if row.Version != 2 || row.Enabled != 0 {
		t.Fatalf("tras v2: version=%d enabled=%d, want 2/0", row.Version, row.Enabled)
	}

	// LWW: una versión MENOR se ignora (no revierte enabled).
	if err := apply(1, true); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	if err := db.Get(&row, q, gymID); err != nil {
		t.Fatalf("read after v1: %v", err)
	}
	if row.Version != 2 || row.Enabled != 0 {
		t.Errorf("LWW: v1 no debió pisar v2, got version=%d enabled=%d", row.Version, row.Enabled)
	}

	// Versión MAYOR sí actualiza (toggle a enabled).
	if err := apply(3, true); err != nil {
		t.Fatalf("apply v3: %v", err)
	}
	if err := db.Get(&row, q, gymID); err != nil {
		t.Fatalf("read after v3: %v", err)
	}
	if row.Version != 3 || row.Enabled != 1 {
		t.Errorf("tras v3: version=%d enabled=%d, want 3/1", row.Version, row.Enabled)
	}
}
