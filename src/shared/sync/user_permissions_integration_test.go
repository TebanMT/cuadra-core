//go:build server && integration

package sync

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Per-operator permissions (UC-007) travel in the users payload. An older
// desktop pushes users without the key: the projector must keep the stored
// value, and both pull paths must send the live column, not the stale
// journal payload.
func TestUserPermissionsSurviveOldDesktopPushAndPull(t *testing.T) {
	db := projectorTestDB(t)
	gymID, ownerID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, _ := tokens.GenerateAccessToken(ownerID, gymID, "owner")
	operatorID := uuid.New()
	email := "op-" + operatorID.String() + "@test.local"
	if err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, phone, role, active, permissions)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'unused', 'Recepción', '+524421234567', 'operator', TRUE, '["credit"]'::jsonb)`,
		operatorID, gymID, email).Error; err != nil {
		t.Fatalf("seed operator: %v", err)
	}

	push := func(version int, extra map[string]any) {
		t.Helper()
		payload := map[string]any{
			"id": operatorID.String(), "gym_id": gymID.String(), "version": version,
			"email": email, "password_hash": "unused", "full_name": "Recepción v" + time.Now().Format("150405.000"),
			"phone": "+524421234567", "role": "operator", "active": true, "must_change_password": false,
			"created_at": time.Now().Add(-time.Hour).UnixMilli(), "updated_at": time.Now().UnixMilli(),
		}
		for k, v := range extra {
			payload[k] = v
		}
		raw, _ := json.Marshal(payload)
		rec := doJSONReq(t, r, http.MethodPost, "/api/v1/sync/push", tok, PushRequest{
			ClientID: uuid.NewString(), ClientNow: time.Now(), SchemaVersion: SchemaVersion,
			Batch: []PushItem{{
				QueueID: uuid.NewString(), EntityType: "users", EntityID: operatorID.String(),
				Operation: OpUpsertStr, ClientVersion: version, Payload: raw, EnqueuedAt: time.Now(),
			}},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("push v%d: %d %s", version, rec.Code, rec.Body.String())
		}
		var pr PushResponse
		if err := json.NewDecoder(rec.Body).Decode(&pr); err != nil || len(pr.Results) != 1 || pr.Results[0].Status != StatusAccepted {
			t.Fatalf("push v%d results: %+v err=%v", version, pr.Results, err)
		}
	}
	stored := func() []string {
		t.Helper()
		var raw string
		if err := db.Raw(`SELECT permissions::text FROM users WHERE id = ?`, operatorID).Scan(&raw).Error; err != nil {
			t.Fatalf("read permissions: %v", err)
		}
		var out []string
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("permissions %q: %v", raw, err)
		}
		return out
	}

	// Old desktop: no permissions key.
	push(2, nil)
	if got := stored(); !reflect.DeepEqual(got, []string{"credit"}) {
		t.Fatalf("old push wiped permissions: %v", got)
	}
	// New desktop: the owner changed them.
	push(3, map[string]any{"permissions": []string{"inventory"}})
	if got := stored(); !reflect.DeepEqual(got, []string{"inventory"}) {
		t.Fatalf("new push permissions = %v", got)
	}
	// Old desktop again: the journal now holds a payload without the key.
	push(4, nil)

	for _, path := range []string{
		"/api/v1/sync/pull?since=" + url.QueryEscape("1970-01-01T00:00:00Z") + "&limit=500",
		"/api/v1/sync/full?limit=5000",
	} {
		rec := doJSONReq(t, r, http.MethodGet, path, tok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		var resp PullResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		found := false
		for _, ch := range resp.Changes {
			if ch.EntityType != "users" || ch.EntityID != operatorID.String() {
				continue
			}
			found = true
			var pl struct {
				Permissions []string `json:"permissions"`
			}
			if err := json.Unmarshal(ch.Payload, &pl); err != nil {
				t.Fatalf("decode users payload: %v", err)
			}
			if !reflect.DeepEqual(pl.Permissions, []string{"inventory"}) {
				t.Errorf("GET %s permissions = %v, want the live [inventory]", path, pl.Permissions)
			}
		}
		if !found {
			t.Errorf("GET %s did not return the operator", path)
		}
	}
}
