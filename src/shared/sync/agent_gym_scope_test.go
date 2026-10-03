//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

func TestAgent_GymSwitchResetsCheckpointAndScopesPendingQueue(t *testing.T) {
	ctx := context.Background()
	cloud := newFakeCloud(t)
	db, uow, oldGymID := setupSidecarDB(t)
	newGymID := uuid.New()
	now := time.Now().UTC()

	// Simulate the legacy global checkpoint written while oldGymID was
	// active. This is the exact shape that skipped the second gym's history.
	if err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		if err := syncpkg.SetInitialSyncCompletedAt(ctx, tx, now); err != nil {
			return err
		}
		if err := syncpkg.SetLastPulledAt(ctx, tx, now); err != nil {
			return err
		}
		return syncpkg.SetLastSyncedAt(ctx, tx, now)
	}); err != nil {
		t.Fatalf("seed old checkpoint: %v", err)
	}

	a := syncpkg.NewAgent(syncpkg.AgentConfig{
		BaseURL: cloud.srv.URL, HTTPClient: cloud.srv.Client(), BatchSize: 50,
	}, db, uow)
	if err := a.SetActiveGym(ctx, newGymID); err != nil {
		t.Fatalf("SetActiveGym: %v", err)
	}
	if err := a.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	state, err := readAgentState(ctx, uow)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if state.ActiveGymID != newGymID {
		t.Fatalf("active gym = %s, want %s", state.ActiveGymID, newGymID)
	}
	if !state.InitialSyncCompletedAt.IsZero() || !state.LastPulledAt.IsZero() || !state.LastSyncedAt.IsZero() {
		t.Fatalf("checkpoint heredado no se limpió: %+v", state)
	}
	if !a.Snapshot().InitialSyncCompletedAt.IsZero() {
		t.Fatal("snapshot todavía considera terminado el full-sync del gym anterior")
	}

	oldEntityID := enqueueScopedTestItem(t, db, oldGymID)
	newEntityID := enqueueScopedTestItem(t, db, newGymID)
	a.SetToken("new-gym-token")
	if err := a.Push(ctx); err != nil {
		t.Fatalf("Push: %v", err)
	}

	cloud.mu.Lock()
	pushCount := len(cloud.pushes)
	batchCount := 0
	var pushedPayload map[string]any
	if pushCount == 1 {
		batchCount = len(cloud.pushes[0].Batch)
		if batchCount == 1 {
			_ = json.Unmarshal(cloud.pushes[0].Batch[0].Payload, &pushedPayload)
		}
	}
	cloud.mu.Unlock()
	if pushCount != 1 || batchCount != 1 {
		t.Fatalf("pushes/batch = %d/%d, want 1/1 para el gym activo", pushCount, batchCount)
	}
	if pushedPayload["gym_id"] != newGymID.String() {
		t.Fatalf("se empujó gym_id=%v, want %s", pushedPayload["gym_id"], newGymID)
	}

	var oldSyncedAt, newSyncedAt *int64
	if err := db.Get(&oldSyncedAt, `SELECT synced_at FROM sync_queue WHERE entity_id = ?`, oldEntityID); err != nil {
		t.Fatalf("old queue: %v", err)
	}
	if err := db.Get(&newSyncedAt, `SELECT synced_at FROM sync_queue WHERE entity_id = ?`, newEntityID); err != nil {
		t.Fatalf("new queue: %v", err)
	}
	if oldSyncedAt != nil {
		t.Fatal("la fila del gym anterior no debe enviarse con la sesión nueva")
	}
	if newSyncedAt == nil {
		t.Fatal("la fila del gym activo no quedó sincronizada")
	}
	if got := a.Snapshot().PendingCount; got != 0 {
		t.Fatalf("PendingCount del gym activo = %d, want 0", got)
	}
}

func TestAgent_SameGymPreservesCompletedCheckpoint(t *testing.T) {
	ctx := context.Background()
	cloud := newFakeCloud(t)
	db, uow, gymID := setupSidecarDB(t)
	want := time.Now().UTC().Truncate(time.Millisecond)
	if err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		if err := syncpkg.SetInitialSyncCompletedAt(ctx, tx, want); err != nil {
			return err
		}
		return syncpkg.SetLastPulledAt(ctx, tx, want)
	}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}

	a := syncpkg.NewAgent(syncpkg.AgentConfig{BaseURL: cloud.srv.URL}, db, uow)
	if err := a.SetActiveGym(ctx, gymID); err != nil {
		t.Fatalf("SetActiveGym same tenant: %v", err)
	}
	if err := a.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	got := a.Snapshot()
	if !got.InitialSyncCompletedAt.Equal(want) || !got.LastPulledAt.Equal(want) {
		t.Fatalf("same-gym checkpoint changed: initial=%v pulled=%v want=%v", got.InitialSyncCompletedAt, got.LastPulledAt, want)
	}
}

func TestAgent_CheckpointFormatUpgradeForcesOneFullSync(t *testing.T) {
	ctx := context.Background()
	cloud := newFakeCloud(t)
	db, uow, gymID := setupSidecarDB(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		if err := syncpkg.SetInitialSyncCompletedAt(ctx, tx, now); err != nil {
			return err
		}
		return syncpkg.SetLastPulledAt(ctx, tx, now)
	}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	// Emulate a binary from before checkpoint materialisation was versioned.
	if _, err := db.Exec(`DELETE FROM sync_state WHERE key = 'sync_checkpoint_format_version'`); err != nil {
		t.Fatalf("remove format key: %v", err)
	}

	a := syncpkg.NewAgent(syncpkg.AgentConfig{BaseURL: cloud.srv.URL}, db, uow)
	if err := a.SetActiveGym(ctx, gymID); err != nil {
		t.Fatalf("SetActiveGym upgrade: %v", err)
	}
	if err := a.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !a.Snapshot().InitialSyncCompletedAt.IsZero() || !a.Snapshot().LastPulledAt.IsZero() {
		t.Fatalf("format upgrade did not reset checkpoint: %+v", a.Snapshot())
	}
	var format string
	if err := db.Get(&format, `SELECT value FROM sync_state WHERE key = 'sync_checkpoint_format_version'`); err != nil {
		t.Fatalf("format key: %v", err)
	}
	if format != "2" {
		t.Fatalf("checkpoint format = %q, want 2", format)
	}
}

func readAgentState(ctx context.Context, uow sharedDomain.UnitOfWork) (syncpkg.State, error) {
	var out syncpkg.State
	err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		var err error
		out, err = syncpkg.ReadState(ctx, tx)
		return err
	})
	return out, err
}

func enqueueScopedTestItem(t *testing.T, db *sqlx.DB, gymID uuid.UUID) string {
	t.Helper()
	entityID := uuid.NewString()
	now := time.Now().UTC().UnixMilli()
	payload, _ := json.Marshal(map[string]any{
		"id": entityID, "gym_id": gymID.String(), "version": 1,
		"created_at": now, "updated_at": now, "full_name": "Scoped member",
	})
	if _, err := db.Exec(`
		INSERT INTO sync_queue
		    (id, entity_type, entity_id, operation, payload, client_version, enqueued_at)
		VALUES (?, 'members', ?, 'upsert', ?, 1, ?)`,
		uuid.NewString(), entityID, string(payload), now); err != nil {
		t.Fatalf("enqueue %s: %v", gymID, err)
	}
	return entityID
}
