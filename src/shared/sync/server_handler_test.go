//go:build server

package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ── Test helpers — in-memory Store + UoW ──────────────────────────────────

type memRow struct {
	gymID, entityType, entityID string
	version                     int
	payload                     json.RawMessage
	serverUpdatedAt             time.Time
	deletedAt                   *time.Time
}

type memStore struct {
	rows           map[string]memRow // key = gym|type|id
	conflicts      []ConflictLogEntry
	failNextUpsert bool
}

func newMemStore() *memStore { return &memStore{rows: map[string]memRow{}} }

func memKey(gymID, entityType, entityID string) string {
	return gymID + "|" + entityType + "|" + entityID
}

func (m *memStore) UpsertOne(ctx context.Context, tx sharedDomain.Transaction, gymID uuid.UUID, item PushItem) (UpsertResult, error) {
	if m.failNextUpsert {
		m.failNextUpsert = false
		return UpsertResult{}, fmt.Errorf("forced db failure")
	}
	var pl map[string]any
	_ = json.Unmarshal(item.Payload, &pl)
	if v, _ := pl["gym_id"].(string); v != gymID.String() {
		return UpsertResult{Status: StatusRejectedUnauthorized}, nil
	}
	now := time.Now().UTC()
	key := memKey(gymID.String(), item.EntityType, item.EntityID)
	existing, exists := m.rows[key]
	if !exists {
		m.rows[key] = memRow{
			gymID: gymID.String(), entityType: item.EntityType, entityID: item.EntityID,
			version: item.ClientVersion, payload: append(json.RawMessage(nil), item.Payload...),
			serverUpdatedAt: now,
		}
		return UpsertResult{Status: StatusAccepted, ServerVersion: item.ClientVersion, ServerUpdatedAt: now}, nil
	}
	if existing.version < item.ClientVersion {
		existing.version = item.ClientVersion
		existing.payload = append(json.RawMessage(nil), item.Payload...)
		existing.serverUpdatedAt = now
		m.rows[key] = existing
		return UpsertResult{Status: StatusAccepted, ServerVersion: item.ClientVersion, ServerUpdatedAt: now}, nil
	}
	clientLocal := extractUpdatedAt(pl)
	if !clientLocalIsAfter(clientLocal, existing.serverUpdatedAt) {
		return UpsertResult{
			Status:                StatusConflictServerWins,
			ServerVersion:         existing.version,
			ServerUpdatedAt:       existing.serverUpdatedAt,
			ServerPayload:         existing.payload,
			IsConflict:            true,
			PreviousServerVersion: existing.version,
			PreviousServerPayload: existing.payload,
		}, nil
	}
	newVersion := existing.version + 1
	if item.ClientVersion > newVersion {
		newVersion = item.ClientVersion + 1
	}
	previousVer := existing.version
	previousPayload := existing.payload
	existing.version = newVersion
	existing.payload = append(json.RawMessage(nil), item.Payload...)
	existing.serverUpdatedAt = now
	m.rows[key] = existing
	return UpsertResult{
		Status:                StatusConflictClientWins,
		ServerVersion:         newVersion,
		ServerUpdatedAt:       now,
		IsConflict:            true,
		PreviousServerVersion: previousVer,
		PreviousServerPayload: previousPayload,
	}, nil
}

func clientLocalIsAfter(clientLocal, serverUpdatedAt time.Time) bool {
	return !clientLocal.IsZero() && clientLocal.After(serverUpdatedAt)
}

func (m *memStore) ListSince(ctx context.Context, tx sharedDomain.Transaction, gymID uuid.UUID, since time.Time, limit int) ([]PullChange, bool, error) {
	out := []PullChange{}
	for _, r := range m.rows {
		if r.gymID != gymID.String() {
			continue
		}
		if !r.serverUpdatedAt.After(since) {
			continue
		}
		out = append(out, PullChange{
			EntityType:      r.entityType,
			EntityID:        r.entityID,
			Version:         r.version,
			Payload:         r.payload,
			ServerUpdatedAt: r.serverUpdatedAt,
			DeletedAt:       r.deletedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ServerUpdatedAt.Equal(out[j].ServerUpdatedAt) {
			return out[i].EntityID < out[j].EntityID
		}
		return out[i].ServerUpdatedAt.Before(out[j].ServerUpdatedAt)
	})
	hasMore := false
	if limit > 0 && len(out) > limit {
		out = out[:limit]
		hasMore = true
	}
	return out, hasMore, nil
}

func (m *memStore) ListForFullSync(ctx context.Context, tx sharedDomain.Transaction, gymID uuid.UUID, cursor FullCursor, limit int) ([]PullChange, FullCursor, bool, error) {
	idx := cursor.TypeIndex
	out := []PullChange{}
	for idx < len(SyncedTables) && len(out) < limit {
		etype := SyncedTables[idx].Type
		typeRows := []memRow{}
		for _, r := range m.rows {
			if r.gymID == gymID.String() && r.entityType == etype {
				typeRows = append(typeRows, r)
			}
		}
		sort.Slice(typeRows, func(i, j int) bool {
			if typeRows[i].serverUpdatedAt.Equal(typeRows[j].serverUpdatedAt) {
				return typeRows[i].entityID < typeRows[j].entityID
			}
			return typeRows[i].serverUpdatedAt.Before(typeRows[j].serverUpdatedAt)
		})
		// Skip already-emitted entries within this type.
		for _, r := range typeRows {
			if idx == cursor.TypeIndex {
				if r.serverUpdatedAt.Before(cursor.After) {
					continue
				}
				if r.serverUpdatedAt.Equal(cursor.After) && r.entityID <= cursor.EntityID {
					continue
				}
			}
			if len(out) >= limit {
				last := out[len(out)-1]
				return out, FullCursor{TypeIndex: idx, After: last.ServerUpdatedAt, EntityID: last.EntityID}, true, nil
			}
			out = append(out, PullChange{
				EntityType: r.entityType, EntityID: r.entityID, Version: r.version,
				Payload: r.payload, ServerUpdatedAt: r.serverUpdatedAt, DeletedAt: r.deletedAt,
			})
		}
		idx++
	}
	return out, FullCursor{TypeIndex: len(SyncedTables)}, false, nil
}

// memUoW — minimal UoW that just calls fn. Sufficient for handler tests
// because memStore doesn't actually need a transaction handle.
type memUoW struct{}

type memTx struct{}

func (memTx) Execute(fn func(tx sharedDomain.Transaction) error) error { return fn(memTx{}) }

func (memUoW) Begin(ctx context.Context) (sharedDomain.Transaction, error) {
	return memTx{}, nil
}
func (memUoW) Commit(_ sharedDomain.Transaction) error   { return nil }
func (memUoW) Rollback(_ sharedDomain.Transaction) error { return nil }
func (memUoW) Query(ctx context.Context) (sharedDomain.Transaction, error) {
	return memTx{}, nil
}
func (memUoW) Command(ctx context.Context, fn func(tx sharedDomain.Transaction) error) error {
	return fn(memTx{})
}

// memConflictLogger — captures conflicts in memory.
type memConflictLogger struct{ entries []ConflictLogEntry }

func (m *memConflictLogger) Log(_ context.Context, _ sharedDomain.Transaction, e ConflictLogEntry) error {
	m.entries = append(m.entries, e)
	return nil
}

// Build a handler bound to in-memory infra. Returns the gin engine + a
// signed JWT for the test gym so the AuthMiddleware accepts requests.
func newTestHandler(t *testing.T) (*gin.Engine, *memStore, uuid.UUID, string, *Metrics) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := newMemStore()
	tokens := auth.NewJWTService("unit-test-secret")
	gymID := uuid.New()
	userID := uuid.New()
	tok, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	metrics := NewMetrics()
	h := NewHandler(memUoW{}, store, &memConflictLogger{}, tokens, nil, metrics)
	r := gin.New()
	h.RegisterRoutes(r)
	return r, store, gymID, tok, metrics
}

// ── Tests ─────────────────────────────────────────────────────────────────

func TestPush_AcceptsNewEntity(t *testing.T) {
	r, store, gymID, tok, _ := newTestHandler(t)
	memberID := uuid.New()
	req := pushBody(gymID, []PushItem{newMemberItem(gymID, memberID, 1, time.Now())})
	resp := doJSON(t, r, "POST", "/api/v1/sync/push", tok, req)
	if resp.Code != 200 {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}
	var pr PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pr.Results) != 1 || pr.Results[0].Status != StatusAccepted {
		t.Fatalf("got %+v", pr.Results)
	}
	if _, ok := store.rows[memKey(gymID.String(), "members", memberID.String())]; !ok {
		t.Fatalf("row not stored")
	}
}

func TestPush_Idempotent(t *testing.T) {
	r, store, gymID, tok, _ := newTestHandler(t)
	memberID := uuid.New()
	now := time.Now()
	req := pushBody(gymID, []PushItem{newMemberItem(gymID, memberID, 1, now)})

	// Push the same item twice.
	doJSON(t, r, "POST", "/api/v1/sync/push", tok, req)
	resp := doJSON(t, r, "POST", "/api/v1/sync/push", tok, req)
	if resp.Code != 200 {
		t.Fatalf("status %d", resp.Code)
	}
	var pr PushResponse
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	// Second push has same client_version → server has version >= client → conflict.
	// LWW by updated_at: identical timestamps → server wins → no mutation.
	if pr.Results[0].Status != StatusConflictServerWins {
		t.Fatalf("expected server_wins, got %s", pr.Results[0].Status)
	}
	if got := store.rows[memKey(gymID.String(), "members", memberID.String())].version; got != 1 {
		t.Errorf("stored version mutated to %d", got)
	}
}

func TestPush_ConflictServerWins(t *testing.T) {
	r, store, gymID, tok, m := newTestHandler(t)
	memberID := uuid.New()

	// First push at version 2 — server stores it.
	tNew := time.Now()
	doJSON(t, r, "POST", "/api/v1/sync/push", tok,
		pushBody(gymID, []PushItem{newMemberItem(gymID, memberID, 2, tNew)}))

	// Second client tries to push version 1 with older timestamp.
	tOld := tNew.Add(-1 * time.Minute)
	resp := doJSON(t, r, "POST", "/api/v1/sync/push", tok,
		pushBody(gymID, []PushItem{newMemberItem(gymID, memberID, 1, tOld)}))
	var pr PushResponse
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	if pr.Results[0].Status != StatusConflictServerWins {
		t.Fatalf("got %s", pr.Results[0].Status)
	}
	if got := store.rows[memKey(gymID.String(), "members", memberID.String())].version; got != 2 {
		t.Errorf("version downgraded to %d", got)
	}
	// Metrics increment for server_wins.
	if !contains(m.Render(), `sync_conflict_total{type="server_wins"} 1`) {
		t.Errorf("metric not incremented:\n%s", m.Render())
	}
}

func TestPush_ConflictClientWins(t *testing.T) {
	r, store, gymID, tok, _ := newTestHandler(t)
	memberID := uuid.New()
	tFirst := time.Now().Add(-1 * time.Hour)
	doJSON(t, r, "POST", "/api/v1/sync/push", tok,
		pushBody(gymID, []PushItem{newMemberItem(gymID, memberID, 1, tFirst)}))

	// Client retries with strictly newer client-local updated_at and same version.
	tSecond := time.Now().Add(time.Hour)
	resp := doJSON(t, r, "POST", "/api/v1/sync/push", tok,
		pushBody(gymID, []PushItem{newMemberItem(gymID, memberID, 1, tSecond)}))
	var pr PushResponse
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	if pr.Results[0].Status != StatusConflictClientWins {
		t.Fatalf("got %s body=%s", pr.Results[0].Status, resp.Body.String())
	}
	if got := store.rows[memKey(gymID.String(), "members", memberID.String())].version; got < 2 {
		t.Errorf("version not bumped on client_wins: %d", got)
	}
}

func TestPush_RejectsUnknownEntityType(t *testing.T) {
	r, _, _, tok, _ := newTestHandler(t)
	resp := doJSON(t, r, "POST", "/api/v1/sync/push", tok, PushRequest{
		ClientID:      uuid.NewString(),
		SchemaVersion: 1,
		Batch: []PushItem{{
			QueueID:       uuid.NewString(),
			EntityType:    "made_up_table",
			EntityID:      uuid.NewString(),
			Operation:     OpUpsertStr,
			ClientVersion: 1,
			Payload:       json.RawMessage(`{}`),
		}},
	})
	var pr PushResponse
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	if pr.Results[0].Status != StatusRejectedUnknownType {
		t.Fatalf("got %s", pr.Results[0].Status)
	}
}

func TestPush_RejectsCrossGym(t *testing.T) {
	r, _, gymID, tok, _ := newTestHandler(t)
	otherGym := uuid.New()
	resp := doJSON(t, r, "POST", "/api/v1/sync/push", tok,
		pushBody(gymID, []PushItem{newMemberItem(otherGym, uuid.New(), 1, time.Now())}))
	var pr PushResponse
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	if pr.Results[0].Status != StatusRejectedUnauthorized {
		t.Fatalf("expected unauthorized, got %s", pr.Results[0].Status)
	}
}

func TestPush_SchemaUpgradeRequired(t *testing.T) {
	r, _, gymID, tok, _ := newTestHandler(t)
	resp := doJSON(t, r, "POST", "/api/v1/sync/push", tok, PushRequest{
		ClientID:      uuid.NewString(),
		SchemaVersion: SchemaVersion + 99,
		Batch:         []PushItem{newMemberItem(gymID, uuid.New(), 1, time.Now())},
	})
	if resp.Code != http.StatusUpgradeRequired {
		t.Fatalf("status %d, want 426", resp.Code)
	}
}

func TestPush_Unauthorized(t *testing.T) {
	r, _, gymID, _, _ := newTestHandler(t)
	resp := doJSON(t, r, "POST", "/api/v1/sync/push", "no-such-token",
		pushBody(gymID, []PushItem{newMemberItem(gymID, uuid.New(), 1, time.Now())}))
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.Code)
	}
}

func TestPull_ReturnsChangesSince(t *testing.T) {
	r, store, gymID, tok, _ := newTestHandler(t)
	// Seed two members in the store.
	for i := 0; i < 2; i++ {
		req := pushBody(gymID, []PushItem{newMemberItem(gymID, uuid.New(), 1, time.Now())})
		doJSON(t, r, "POST", "/api/v1/sync/push", tok, req)
	}
	if len(store.rows) != 2 {
		t.Fatalf("expected 2 rows, have %d", len(store.rows))
	}
	resp := doJSON(t, r, "GET", "/api/v1/sync/pull?since=", tok, nil)
	var pr PullResponse
	_ = json.NewDecoder(resp.Body).Decode(&pr)
	if len(pr.Changes) != 2 {
		t.Errorf("changes=%d", len(pr.Changes))
	}
}

func TestFullSync_Pagination(t *testing.T) {
	r, _, gymID, tok, _ := newTestHandler(t)
	// Seed 5 members.
	for i := 0; i < 5; i++ {
		req := pushBody(gymID, []PushItem{newMemberItem(gymID, uuid.New(), 1, time.Now().Add(time.Duration(i)*time.Millisecond))})
		doJSON(t, r, "POST", "/api/v1/sync/push", tok, req)
	}
	// First page with limit=2.
	resp := doJSON(t, r, "GET", "/api/v1/sync/full?limit=2", tok, nil)
	var first FullSyncResponse
	_ = json.NewDecoder(resp.Body).Decode(&first)
	if len(first.Changes) != 2 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("bad first page: %+v", first)
	}
	// Second page using the cursor.
	resp = doJSON(t, r, "GET", "/api/v1/sync/full?limit=10&cursor="+first.NextCursor, tok, nil)
	var second FullSyncResponse
	_ = json.NewDecoder(resp.Body).Decode(&second)
	if len(second.Changes) != 3 {
		t.Errorf("second page changes=%d", len(second.Changes))
	}
	if second.HasMore {
		t.Errorf("second page should be final")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	r, _, gymID, tok, _ := newTestHandler(t)
	doJSON(t, r, "POST", "/api/v1/sync/push", tok,
		pushBody(gymID, []PushItem{newMemberItem(gymID, uuid.New(), 1, time.Now())}))
	resp := doJSON(t, r, "GET", "/_internal/metrics", "", nil)
	if resp.Code != 200 {
		t.Errorf("metrics status %d", resp.Code)
	}
	body := resp.Body.String()
	if !contains(body, "sync_push_requests_total") || !contains(body, "sync_request_duration_seconds") {
		t.Errorf("missing expected metric series:\n%s", body)
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────

func newMemberItem(gymID, memberID uuid.UUID, version int, updatedAt time.Time) PushItem {
	payload := map[string]any{
		"id":         memberID.String(),
		"gym_id":     gymID.String(),
		"version":    version,
		"updated_at": updatedAt.UnixMilli(),
		"folio":      "M-001",
		"full_name":  "Test Member",
		"phone":      "5555",
		"status":     "active",
	}
	b, _ := json.Marshal(payload)
	return PushItem{
		QueueID:       uuid.NewString(),
		EntityType:    "members",
		EntityID:      memberID.String(),
		Operation:     OpUpsertStr,
		ClientVersion: version,
		Payload:       b,
		EnqueuedAt:    time.Now(),
	}
}

func pushBody(_ uuid.UUID, items []PushItem) PushRequest {
	return PushRequest{
		ClientID:      uuid.NewString(),
		ClientNow:     time.Now(),
		SchemaVersion: 1,
		Batch:         items,
	}
}

func doJSON(t *testing.T, r http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequest(method, path, buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}
