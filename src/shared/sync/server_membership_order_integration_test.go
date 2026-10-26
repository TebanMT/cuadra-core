//go:build server && integration

package sync

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestFinancialGraphRenewalPredecessorFirst(t *testing.T) {
	db := projectorTestDB(t)
	gym, owner := seedGymAndOwner(t, db)
	member, kind, old, next := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO members(id,gym_id,folio,full_name,phone,status,created_by) VALUES(?,?,?,'Renewal test','5551234567','active',?)`, member, gym, member.String(), owner).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO membership_types(id,gym_id,name,price,duration_days) VALUES(?,?,'Renewal test',500,30)`, kind, gym).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	batch := []PushItem{}
	keys := []string{expenseMemberKey("memberships", old.String()), expenseMemberKey("memberships", next.String())}
	for i, id := range []uuid.UUID{old, next} {
		raw := map[string]any{"id": id, "gym_id": gym, "version": 1, "created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "member_id": member, "membership_type_id": kind, "type_name_snapshot": "Mensual", "price_snapshot": 500, "duration_days_snapshot": 30, "start_date": "2026-10-01", "expiry_date": "2026-10-31", "status": "active", financialMembersKey: keys}
		if i == 0 {
			raw["status"] = "replaced"
			raw["replaced_by"] = next
		}
		payload, _ := json.Marshal(raw)
		batch = append(batch, PushItem{QueueID: uuid.NewString(), EntityType: "memberships", EntityID: id.String(), Operation: OpUpsertStr, ClientVersion: 1, Payload: payload, EnqueuedAt: now})
	}
	router, tokens := newRealHandler(t, db)
	token, _ := tokens.GenerateAccessToken(owner, gym, "owner")
	resp, status, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: batch})
	if err != nil || status != 200 {
		t.Fatalf("push: %d %v", status, err)
	}
	for _, result := range resp.Results {
		if result.Status != StatusAccepted {
			t.Fatalf("renewal rejected: %+v", result)
		}
	}
	var target string
	if err := db.Raw(`SELECT replaced_by FROM memberships WHERE id=? AND gym_id=?`, old, gym).Scan(&target).Error; err != nil {
		t.Fatal(err)
	}
	if target != next.String() {
		t.Fatalf("replaced_by=%s, want %s", target, next)
	}
}
