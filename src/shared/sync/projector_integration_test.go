//go:build server && integration

package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// projectorTestDB lazily opens the integration Postgres handle. Tests that
// need it call this; if DATABASE_URL is missing the suite skips so CI on
// machines without Postgres remains green.
func projectorTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/cuadra?sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Skipf("integration test skipped — cannot reach Postgres at %s: %v", dsn, err)
	}
	return db
}

// seedGymAndOwner inserts a fresh gym + owner user via raw SQL so the
// integration tests aren't coupled to the use-case layer. Returns
// (gymID, userID). t.Cleanup deletes everything tagged with that gym_id at
// the end of the test, scoped tight enough that parallel runs don't
// collide.
func seedGymAndOwner(t *testing.T, db *gorm.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	gymID := uuid.New()
	userID := uuid.New()

	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
		VALUES (?, ?, 1, NOW(), NOW(), 'Projector Test Gym', 'MX', 'America/Mexico_City')`,
		gymID, gymID).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at, email, password_hash, full_name, role, active)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'unused', 'Projector Owner', 'owner', TRUE)`,
		userID, gymID, "owner-"+gymID.String()+"@test.local").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	t.Cleanup(func() {
		// Truncate per-gym data in reverse FK order. Tests can leak rows
		// across runs if cleanup fails — use IF EXISTS guards on each.
		// audit_log doesn't have FKs into gyms but is gym-scoped via gym_id.
		for _, table := range []string{
			"sync_entities", "conflict_log",
			"audit_log", "notification_queue",
			"checkins", "stock_movements", "sale_items", "sales", "payments",
			"contact_attempts", "member_fingerprints",
			"membership_adjustments", "memberships", "members",
			"membership_types", "products",
			"cash_close_events", "gym_ownership_transfers",
			"users", "gyms",
		} {
			_ = db.Exec("DELETE FROM "+table+" WHERE gym_id = ?", gymID).Error
		}
	})
	return gymID, userID
}

// newRealHandler wires the cloud sync handler against the real Postgres UoW.
func newRealHandler(t *testing.T, db *gorm.DB) (*gin.Engine, auth.TokenService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	tokens := auth.NewJWTService("integration-test-secret")
	h := NewHandler(uow, NewPostgresStore(), NewConflictLogger(), tokens, nil, NewMetrics())
	r := gin.New()
	h.RegisterRoutes(r)
	return r, tokens
}

func doJSONReq(t *testing.T, r http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
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

// TestPushAppliesToProjector_Member is the user-visible bug from ADR-008
// §1.1: push of a `members` entity must land in the `members` domain table,
// not just sync_entities. Run with -tags 'server integration'.
func TestPushAppliesToProjector_Member(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	memberID := uuid.New()
	memberPayload := map[string]any{
		"id":              memberID.String(),
		"gym_id":          gymID.String(),
		"version":         1,
		"folio":           "P-001",
		"full_name":       "Integration Test Member",
		"phone":           "5550000",
		"status":          "active",
		"enrollment_paid": false,
		"created_by":      userID.String(),
		"updated_at":      time.Now().UnixMilli(),
	}
	pb, _ := json.Marshal(memberPayload)
	req := PushRequest{
		ClientID:      uuid.NewString(),
		ClientNow:     time.Now(),
		SchemaVersion: 1,
		Batch: []PushItem{{
			QueueID:       uuid.NewString(),
			EntityType:    "members",
			EntityID:      memberID.String(),
			Operation:     OpUpsertStr,
			ClientVersion: 1,
			Payload:       pb,
			EnqueuedAt:    time.Now(),
		}},
	}

	resp := doJSONReq(t, r, "POST", "/api/v1/sync/push", tok, req)
	if resp.Code != 200 {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}
	var pr PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pr.Results) != 1 || pr.Results[0].Status != StatusAccepted {
		t.Fatalf("results: %+v", pr.Results)
	}

	// Domain table must now contain the member.
	var got struct {
		ID       uuid.UUID
		FullName string
		Status   string
		Version  int
	}
	if err := db.Raw(
		`SELECT id, full_name, status, version FROM members WHERE id = ? AND gym_id = ?`,
		memberID, gymID,
	).Scan(&got).Error; err != nil {
		t.Fatalf("query members: %v", err)
	}
	if got.ID != memberID {
		t.Errorf("member missing from domain table — id=%v want=%v", got.ID, memberID)
	}
	if got.FullName != "Integration Test Member" {
		t.Errorf("full_name = %q", got.FullName)
	}
	if got.Status != "active" {
		t.Errorf("status = %q", got.Status)
	}
	if got.Version != 1 {
		t.Errorf("version = %d", got.Version)
	}

	// And sync_entities still has its mirror row (sanity — the projector is
	// additive, not a replacement).
	var n int
	if err := db.Raw(
		`SELECT COUNT(*) FROM sync_entities WHERE gym_id = ? AND entity_type = 'members' AND entity_id = ?`,
		gymID, memberID,
	).Scan(&n).Error; err != nil {
		t.Fatalf("count sync_entities: %v", err)
	}
	if n != 1 {
		t.Errorf("sync_entities row missing: count=%d", n)
	}
}

func TestPushAppliesToProjector_UserUpdate(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, _ := tokens.GenerateAccessToken(userID, gymID, "owner")

	// Push at version 2 (greater than the seeded version 1) — projector
	// must UPDATE the user row in place via ON CONFLICT.
	body := map[string]any{
		"id":            userID.String(),
		"gym_id":        gymID.String(),
		"version":       2,
		"email":         "owner-" + gymID.String() + "@test.local", // same as seeded
		"password_hash": "new-hash",
		"full_name":     "Renamed Owner",
		"role":          "owner",
		"active":        true,
		"updated_at":    time.Now().UnixMilli(),
	}
	pb, _ := json.Marshal(body)
	req := PushRequest{
		ClientID:      uuid.NewString(),
		ClientNow:     time.Now(),
		SchemaVersion: 1,
		Batch: []PushItem{{
			QueueID: uuid.NewString(), EntityType: "users", EntityID: userID.String(),
			Operation: OpUpsertStr, ClientVersion: 2, Payload: pb, EnqueuedAt: time.Now(),
		}},
	}
	resp := doJSONReq(t, r, "POST", "/api/v1/sync/push", tok, req)
	if resp.Code != 200 {
		t.Fatalf("status %d: %s", resp.Code, resp.Body.String())
	}

	var got struct {
		FullName     string
		PasswordHash string
		Version      int
	}
	if err := db.Raw(`SELECT full_name, password_hash, version FROM users WHERE id = ?`, userID).Scan(&got).Error; err != nil {
		t.Fatalf("read user: %v", err)
	}
	if got.FullName != "Renamed Owner" || got.PasswordHash != "new-hash" || got.Version != 2 {
		t.Errorf("user not updated by projector: %+v", got)
	}
}

func TestPushAppliesToProjector_SoftDelete(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, _ := tokens.GenerateAccessToken(userID, gymID, "owner")

	memberID := uuid.New()
	// Insert.
	insertPayload, _ := json.Marshal(map[string]any{
		"id": memberID.String(), "gym_id": gymID.String(), "version": 1,
		"folio": "DEL-001", "full_name": "To Be Deleted", "phone": "555", "status": "active",
		"enrollment_paid": false, "created_by": userID.String(),
		"updated_at": time.Now().UnixMilli(),
	})
	req1 := PushRequest{
		ClientID: uuid.NewString(), ClientNow: time.Now(), SchemaVersion: 1,
		Batch: []PushItem{{
			QueueID: uuid.NewString(), EntityType: "members", EntityID: memberID.String(),
			Operation: OpUpsertStr, ClientVersion: 1, Payload: insertPayload, EnqueuedAt: time.Now(),
		}},
	}
	if r1 := doJSONReq(t, r, "POST", "/api/v1/sync/push", tok, req1); r1.Code != 200 {
		t.Fatalf("insert status %d: %s", r1.Code, r1.Body.String())
	}

	// Soft-delete via Operation=delete. The sidecar sends operation=delete
	// even with the same payload; UpsertOne sets deleted_at = NOW() in
	// sync_entities, but the projector still applies the payload to the
	// domain row — domain row keeps deleted_at NULL unless the payload
	// includes it. Re-issue with payload that carries deleted_at directly
	// (matches what the sidecar's delete path currently sends — see
	// ADR-008 §3.1 soft-delete clause).
	deletedMs := time.Now().UnixMilli()
	deletePayload, _ := json.Marshal(map[string]any{
		"id": memberID.String(), "gym_id": gymID.String(), "version": 2,
		"folio": "DEL-001", "full_name": "To Be Deleted", "phone": "555", "status": "inactive",
		"enrollment_paid": false, "created_by": userID.String(),
		"deleted_at": time.UnixMilli(deletedMs).UTC().Format(time.RFC3339Nano),
		"updated_at": deletedMs,
	})
	req2 := PushRequest{
		ClientID: uuid.NewString(), ClientNow: time.Now(), SchemaVersion: 1,
		Batch: []PushItem{{
			QueueID: uuid.NewString(), EntityType: "members", EntityID: memberID.String(),
			Operation: OpUpsertStr, ClientVersion: 2, Payload: deletePayload, EnqueuedAt: time.Now(),
		}},
	}
	if r2 := doJSONReq(t, r, "POST", "/api/v1/sync/push", tok, req2); r2.Code != 200 {
		t.Fatalf("delete status %d: %s", r2.Code, r2.Body.String())
	}

	var got struct {
		DeletedAt *time.Time
	}
	if err := db.Raw(`SELECT deleted_at FROM members WHERE id = ?`, memberID).Scan(&got).Error; err != nil {
		t.Fatalf("read member: %v", err)
	}
	if got.DeletedAt == nil {
		t.Errorf("expected deleted_at to be non-nil after soft-delete projector")
	}
}

func TestPushAppliesToProjector_GymJSONB(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, _ := tokens.GenerateAccessToken(userID, gymID, "owner")

	// Update the gym with a payment_methods JSONB array — must round-trip
	// through ?::jsonb.
	body := map[string]any{
		"id":              gymID.String(),
		"gym_id":          gymID.String(),
		"version":         2,
		"name":            "Renamed Gym",
		"payment_methods": []string{"cash", "transfer"},
		"updated_at":      time.Now().UnixMilli(),
	}
	pb, _ := json.Marshal(body)
	req := PushRequest{
		ClientID: uuid.NewString(), ClientNow: time.Now(), SchemaVersion: 1,
		Batch: []PushItem{{
			QueueID: uuid.NewString(), EntityType: "gyms", EntityID: gymID.String(),
			Operation: OpUpsertStr, ClientVersion: 2, Payload: pb, EnqueuedAt: time.Now(),
		}},
	}
	if rec := doJSONReq(t, r, "POST", "/api/v1/sync/push", tok, req); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Name           string
		PaymentMethods []byte // JSONB read as bytes
	}
	if err := db.Raw(
		`SELECT name, payment_methods::text AS payment_methods FROM gyms WHERE id = ?`,
		gymID,
	).Scan(&got).Error; err != nil {
		t.Fatalf("read gym: %v", err)
	}
	if got.Name != "Renamed Gym" {
		t.Errorf("name = %q", got.Name)
	}
	if !strings.Contains(string(got.PaymentMethods), "cash") || !strings.Contains(string(got.PaymentMethods), "transfer") {
		t.Errorf("payment_methods JSONB lost: %s", got.PaymentMethods)
	}
}

// TestPushRejectsCrossGymAtProjector — the projector must NOT allow a
// payload claiming a different gym to leak through. UpsertOne already
// rejects cross-gym at status=rejected_unauthorized; this test pins the
// behaviour end-to-end (the projector is never called for that path).
func TestPushRejectsCrossGymAtProjector(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, _ := tokens.GenerateAccessToken(userID, gymID, "owner")

	otherGym := uuid.New()
	body := map[string]any{
		"id": uuid.NewString(), "gym_id": otherGym.String(), "version": 1,
		"folio": "X", "full_name": "X", "phone": "x", "status": "active",
		"enrollment_paid": false, "created_by": userID.String(),
		"updated_at": time.Now().UnixMilli(),
	}
	pb, _ := json.Marshal(body)
	req := PushRequest{
		ClientID: uuid.NewString(), ClientNow: time.Now(), SchemaVersion: 1,
		Batch: []PushItem{{
			QueueID: uuid.NewString(), EntityType: "members", EntityID: uuid.NewString(),
			Operation: OpUpsertStr, ClientVersion: 1, Payload: pb, EnqueuedAt: time.Now(),
		}},
	}
	rec := doJSONReq(t, r, "POST", "/api/v1/sync/push", tok, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var pr PushResponse
	_ = json.NewDecoder(rec.Body).Decode(&pr)
	if pr.Results[0].Status != StatusRejectedUnauthorized {
		t.Errorf("expected unauthorized, got %s", pr.Results[0].Status)
	}
	// Nothing leaked into the other gym's table.
	var n int
	_ = db.Raw(`SELECT COUNT(*) FROM members WHERE gym_id = ?`, otherGym).Scan(&n)
	if n != 0 {
		t.Errorf("cross-gym row leaked into members: %d rows", n)
	}
	_ = context.TODO() // silence import if not used elsewhere
}
