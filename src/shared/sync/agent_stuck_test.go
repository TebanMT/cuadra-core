//go:build sidecar

package sync

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Pin del conteo de filas atoradas (caso real: 6 filas de socios pre-corte
// rechazadas per-item tras la migración, con el indicador en "Sincronizado").
// refreshPendingCount debe distinguir pendientes normales (retry_count bajo,
// van a salir) de atoradas (>= stuckPushThreshold, el server las rechaza
// determinísticamente) y subir el last_error de la más castigada.
func TestRefreshPendingCount_StuckRows(t *testing.T) {
	gymID := uuid.New()
	db, uow := compositeKeyTestDB(t, gymID)
	a := NewAgent(AgentConfig{BaseURL: "http://x"}, db, uow)
	ctx := context.Background()

	nowMs := time.Now().UTC().UnixMilli()
	seed := func(retry int, lastErr string) {
		t.Helper()
		if _, err := db.Exec(`
			INSERT INTO sync_queue (id, entity_type, entity_id, operation, payload, client_version, enqueued_at, retry_count, last_error)
			VALUES (?, 'members', ?, 'upsert', '{}', 1, ?, ?, ?)`,
			uuid.New().String(), uuid.New().String(), nowMs, retry, lastErr); err != nil {
			t.Fatalf("seed queue: %v", err)
		}
	}
	seed(0, "")                                        // pendiente fresco
	seed(1, "blip transitorio")                        // aún no atorado
	seed(5, "rejected_internal_error: fk members")     // atorado
	seed(9, "rejected_internal_error: fk memberships") // el más castigado

	a.refreshPendingCount(ctx)
	snap := a.Snapshot()

	if snap.PendingCount != 4 {
		t.Errorf("PendingCount = %d, want 4", snap.PendingCount)
	}
	if snap.StuckPushCount != 2 {
		t.Errorf("StuckPushCount = %d, want 2 (sólo retry_count >= %d)", snap.StuckPushCount, stuckPushThreshold)
	}
	if snap.StuckPushError != "rejected_internal_error: fk memberships" {
		t.Errorf("StuckPushError = %q, want el de la fila más castigada", snap.StuckPushError)
	}

	// Al drenar la cola, el estado se limpia.
	if _, err := db.Exec(`UPDATE sync_queue SET synced_at = ?`, nowMs); err != nil {
		t.Fatalf("drain: %v", err)
	}
	a.refreshPendingCount(ctx)
	snap = a.Snapshot()
	if snap.PendingCount != 0 || snap.StuckPushCount != 0 {
		t.Errorf("tras drenar: pending=%d stuck=%d, want 0/0", snap.PendingCount, snap.StuckPushCount)
	}
}
