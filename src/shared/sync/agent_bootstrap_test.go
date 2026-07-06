//go:build sidecar

package sync_test

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestBootstrap_NoHeredaBackoffPersistido — pin del bug post auto-update del
// dogfood: el sidecar v1.0.6 acumuló backoff fallando el full-sync toda la
// noche; tras actualizar a v1.0.7 el proceso NUEVO restauraba NextRetryAt de
// sync_state y se quedaba mudo hasta 5 min (sin nada en la UI que lo
// explicara), aunque el binario nuevo ya traía el fix del fallo. Un proceso
// fresco debe intentar de inmediato. ConsecutiveFailures SÍ se restaura: si
// el fallo persiste, recordFailure re-arma el backoff directo en el cap en
// vez de martillar desde 1s.
func TestBootstrap_NoHeredaBackoffPersistido(t *testing.T) {
	cloud := newFakeCloud(t)
	db, uow, gymID := setupSidecarDB(t)
	enqueueMember(t, db, gymID, 1)

	// Estado heredado de un proceso anterior que venía fallando.
	future := time.Now().Add(4 * time.Minute).UnixMilli()
	for k, v := range map[string]string{
		"next_retry_at_ms":     strconv.FormatInt(future, 10),
		"consecutive_failures": "40",
	} {
		if _, err := db.Exec(`INSERT OR REPLACE INTO sync_state (key, value, updated_at) VALUES (?, ?, ?)`,
			k, v, time.Now().UTC().UnixMilli()); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	a := newAgent(t, cloud, db, uow, "tok") // newAgent corre Bootstrap

	snap := a.Snapshot()
	if !snap.NextRetryAt.IsZero() {
		t.Fatalf("NextRetryAt heredado tras bootstrap: %v — un arranque fresco debe intentar ya", snap.NextRetryAt)
	}
	if snap.ConsecutiveFailures != 40 {
		t.Errorf("ConsecutiveFailures = %d, want 40 (se preserva para que el backoff re-arme en el cap)", snap.ConsecutiveFailures)
	}

	// Y de verdad corre: el primer RunOnce empuja la cola al cloud en vez
	// de respetar el retraso del proceso muerto.
	a.RunOnce(context.Background())
	cloud.mu.Lock()
	pushes := len(cloud.pushes)
	cloud.mu.Unlock()
	if pushes == 0 {
		t.Fatal("RunOnce no empujó nada — sigue respetando el backoff heredado")
	}
}
