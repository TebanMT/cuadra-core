//go:build sidecar

package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A historical membership_types journal wrote cents on the wire. The normal
// incremental LWW correctly skips an equal version, but a versioned full-sync
// must be able to re-materialise the canonical payload at that same version.
func TestFullSyncPage_EqualVersionRepairsCanonicalRow(t *testing.T) {
	ctx := context.Background()
	gymID := uuid.New()
	db, uow := compositeKeyTestDB(t, gymID)
	typeID := uuid.New()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := db.Exec(`
		INSERT INTO membership_types
		    (id, gym_id, version, created_at, updated_at, synced_at,
		     name, price, duration_days, duration_months,
		     enrollment_fee, maintenance_fee, active)
		VALUES (?, ?, 1, ?, ?, ?, 'Normal', 4500000, 30, 1, 0, 0, 1)`,
		typeID, gymID, now.UnixMilli(), now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatalf("seed poisoned row: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"id": typeID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(),
		"name": "Normal", "price": 450.0, "duration_days": 30, "duration_months": 1,
		"enrollment_fee": 0, "maintenance_fee": 0,
		"maintenance_frequency": nil, "active": true,
	})
	change := PullChange{
		EntityType: "membership_types", EntityID: typeID.String(), Version: 1,
		Payload: payload, ServerUpdatedAt: now.Add(time.Minute),
	}

	if err := ApplyPullPage(ctx, uow, []PullChange{change}, nil); err != nil {
		t.Fatalf("incremental apply: %v", err)
	}
	var cents int
	_ = db.Get(&cents, `SELECT price FROM membership_types WHERE id = ?`, typeID)
	if cents != 4500000 {
		t.Fatalf("incremental equal-version apply changed LWW row: %d", cents)
	}

	if err := applyFullSyncPage(ctx, uow, []PullChange{change}, nil); err != nil {
		t.Fatalf("full-sync apply: %v", err)
	}
	_ = db.Get(&cents, `SELECT price FROM membership_types WHERE id = ?`, typeID)
	if cents != 45000 {
		t.Fatalf("full-sync repair price = %d cents, want 45000", cents)
	}
}

func TestFullSyncPage_EqualVersionPreservesPendingExactEntity(t *testing.T) {
	ctx := context.Background()
	gymID := uuid.New()
	db, uow := compositeKeyTestDB(t, gymID)
	typeID := uuid.New()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := db.Exec(`
		INSERT INTO membership_types
		    (id, gym_id, version, created_at, updated_at,
		     name, price, duration_days, duration_months,
		     enrollment_fee, maintenance_fee, active)
		VALUES (?, ?, 1, ?, ?, 'Local edit', 40000, 30, 1, 0, 0, 1)`,
		typeID, gymID, now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatalf("seed pending row: %v", err)
	}
	localPayload, _ := json.Marshal(map[string]any{
		"id": typeID.String(), "gym_id": gymID.String(), "version": 1,
		"name": "Local edit", "price": 400.0, "duration_days": 30,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "active": true,
	})
	// Raw queue insert keeps the fixture focused on the pending-item guard.
	if _, err := db.Exec(`
		INSERT INTO sync_queue
		    (id, entity_type, entity_id, operation, payload, client_version, enqueued_at)
		VALUES (?, 'membership_types', ?, 'upsert', ?, 1, ?)`,
		uuid.New(), typeID, string(localPayload), now.UnixMilli()); err != nil {
		t.Fatalf("seed pending queue: %v", err)
	}
	serverPayload, _ := json.Marshal(map[string]any{
		"id": typeID.String(), "gym_id": gymID.String(), "version": 1,
		"name": "Server old", "price": 450.0, "duration_days": 30,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "active": true,
	})
	if err := applyFullSyncPage(ctx, uow, []PullChange{{
		EntityType: "membership_types", EntityID: typeID.String(), Version: 1,
		Payload: serverPayload, ServerUpdatedAt: now.Add(time.Minute),
	}}, nil); err != nil {
		t.Fatalf("full-sync pending guard: %v", err)
	}
	var name string
	_ = db.Get(&name, `SELECT name FROM membership_types WHERE id = ?`, typeID)
	if name != "Local edit" {
		t.Fatalf("pending local row overwritten with %q", name)
	}
}
