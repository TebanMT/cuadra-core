//go:build server && integration

package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// projectorTestDB lazily opens the integration Postgres handle with the
// schema migrated to head. Tests that need it call this; if Postgres is
// unreachable the suite skips so CI on machines without Postgres stays green.
func projectorTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.OpenPostgres(t)
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
			"cash_transfers", "cash_close_events", "refund_items", "refunds", "sale_corrections", "payment_corrections",
			"inventory_purchases",
			"expenses", "expense_occurrences", "cash_movements", "recurring_expense_templates",
			"checkins", "stock_movements", "sale_items", "sales", "payments",
			"contact_attempts", "member_fingerprints",
			"membership_adjustments", "memberships", "members",
			"membership_types", "products", "cash_drawers",
			"gym_ownership_transfers",
			"users", "gyms",
		} {
			_ = db.Exec("DELETE FROM "+table+" WHERE gym_id = ?", gymID).Error
		}
	})
	return gymID, userID
}

func TestPushCashSession_AcceptsSQLiteBooleanIntegers(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	sessionID := uuid.New()
	payload := mustJSON(t, map[string]any{
		"id": sessionID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
		"drawer_id": gymID.String(), "drawer_code": "main", "operational_date": "2026-08-25",
		"sequence": 1, "status": "reconciled", "close_date": "2026-08-25",
		"opening_cash": 0.0, "opening_cash_known": 1,
		"activity_cash": -9800.0, "calculated_cash": -9800.0, "counted_cash": 0.0,
		"cash_left": nil, "withdrawn_cash": nil, "withdrawal_destination": nil,
		"discrepancy_reason": "Prueba de cierre", "correction_reason": nil,
		"adjusted_after_withdrawal": 0, "integrity_note": nil,
		"opened_at": now.UnixMilli(), "opened_by": userID.String(),
		"closed_at": now.UnixMilli(), "closed_by": userID.String(),
		"reconciled_at": now.UnixMilli(), "reconciled_by": userID.String(),
		"stale_at": nil, "withdrawn_at": nil, "withdrawn_by": nil,
	})
	response, status, err := performSyncPush(router, token, PushRequest{
		ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion,
		Batch: []PushItem{{
			QueueID: uuid.NewString(), EntityType: "cash_close_events", EntityID: sessionID.String(),
			Operation: OpUpsertStr, ClientVersion: 1, Payload: payload, EnqueuedAt: now,
		}},
	})
	if err != nil || status != http.StatusOK || len(response.Results) != 1 || response.Results[0].Status != StatusAccepted {
		t.Fatalf("push status=%d err=%v response=%+v", status, err, response)
	}
	var row struct {
		OpeningKnown bool    `gorm:"column:opening_cash_known"`
		Adjusted     bool    `gorm:"column:adjusted_after_withdrawal"`
		Activity     float64 `gorm:"column:activity_cash"`
	}
	if err := db.Raw(`SELECT opening_cash_known,adjusted_after_withdrawal,activity_cash
		FROM cash_close_events WHERE gym_id=? AND id=?`, gymID, sessionID).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if !row.OpeningKnown || row.Adjusted || row.Activity != -9800 {
		t.Fatalf("projected session=%+v, want true/false/-9800", row)
	}
}

func TestPushAppliesToProjector_ExpensesPlusGraphAndTombstone(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	ms := now.UnixMilli()
	templateID, occurrenceID, movementID, expenseID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	encode := func(value map[string]any) json.RawMessage {
		t.Helper()
		payload, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return payload
	}
	item := func(entityType string, id uuid.UUID, payload map[string]any) PushItem {
		return PushItem{QueueID: uuid.NewString(), EntityType: entityType, EntityID: id.String(), Operation: OpUpsertStr, ClientVersion: 1, Payload: encode(payload), EnqueuedAt: now}
	}
	base := func(id uuid.UUID) map[string]any {
		return map[string]any{"id": id, "gym_id": gymID, "version": 1, "created_at": ms, "updated_at": ms}
	}
	template := base(templateID)
	template["name"], template["category"], template["expected_amount"] = "Renta", "renta", 1234.56
	template["usual_payment_method"], template["classification"], template["frequency"] = "cash", "fixed", "monthly"
	template["starts_on"], template["next_due_on"], template["active"], template["created_by"] = "2026-08-13", "2026-09-13", true, userID
	occurrence := base(occurrenceID)
	occurrence["template_id"], occurrence["due_on"], occurrence["expected_amount"] = templateID, "2026-08-13", 1234.56
	occurrence["category"], occurrence["payment_method"], occurrence["classification"], occurrence["status"] = "renta", "cash", "fixed", "paid"
	occurrence["expense_id"], occurrence["resolved_by"], occurrence["resolved_at"] = expenseID, userID, ms
	movement := base(movementID)
	movement["movement_on"], movement["amount"], movement["movement_type"] = "2026-08-13", 1234.56, "cash_out"
	movement["reason"], movement["operator_id"], movement["expense_id"], movement["classification_status"] = "Renta", userID, expenseID, "expense"
	expense := base(expenseID)
	expense["expense_date"], expense["amount"], expense["category"] = "2026-08-13", 1234.56, "renta"
	expense["payment_method"], expense["created_by"], expense["classification"], expense["source"] = "cash", userID, "fixed", "recurring"
	expense["paid_from"] = "cash_register"
	expense["recurring_occurrence_id"], expense["cash_movement_id"] = occurrenceID, movementID

	batch := []PushItem{
		item("recurring_expense_templates", templateID, template),
		item("expense_occurrences", occurrenceID, occurrence),
		item("cash_movements", movementID, movement),
		item("expenses", expenseID, expense),
	}
	rec := doJSONReq(t, r, http.MethodPost, "/api/v1/sync/push", tok, PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion, Batch: batch})
	if rec.Code != http.StatusOK {
		t.Fatalf("push status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response PushResponse
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 4 {
		t.Fatalf("push results=%+v", response.Results)
	}
	for _, result := range response.Results {
		if result.Status != StatusAccepted {
			t.Fatalf("push result=%+v", result)
		}
	}
	var amount string
	var occurrenceClass, cashLink string
	if err = db.Raw(`SELECT amount::text, classification, cash_movement_id::text FROM expenses WHERE gym_id=? AND id=?`, gymID, expenseID).Row().Scan(&amount, &occurrenceClass, &cashLink); err != nil {
		t.Fatal(err)
	}
	if amount != "1234.56" || occurrenceClass != "fixed" || cashLink != movementID.String() {
		t.Fatalf("amount=%s class=%s cash_link=%s", amount, occurrenceClass, cashLink)
	}
	for _, table := range []string{"recurring_expense_templates", "expense_occurrences", "cash_movements", "expenses"} {
		var count int64
		if err = db.Table(table).Where("gym_id=?", gymID).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}

	deleted := now.Add(time.Second)
	expense["version"], expense["updated_at"], expense["deleted_at"] = 2, deleted.UnixMilli(), deleted.UnixMilli()
	deleteItem := item("expenses", expenseID, expense)
	deleteItem.Operation, deleteItem.ClientVersion, deleteItem.EnqueuedAt = OpDeleteStr, 2, deleted
	movement["version"], movement["updated_at"], movement["deleted_at"] = 2, deleted.UnixMilli(), deleted.UnixMilli()
	movementDelete := item("cash_movements", movementID, movement)
	movementDelete.Operation, movementDelete.ClientVersion = OpDeleteStr, 2
	occurrence["version"], occurrence["updated_at"], occurrence["status"] = 2, deleted.UnixMilli(), "pending"
	occurrence["expense_id"], occurrence["resolved_by"], occurrence["resolved_at"] = nil, nil, nil
	reopened := item("expense_occurrences", occurrenceID, occurrence)
	reopened.ClientVersion = 2
	rec = doJSONReq(t, r, http.MethodPost, "/api/v1/sync/push", tok, PushRequest{ClientID: uuid.NewString(), ClientNow: deleted, SchemaVersion: SchemaVersion, Batch: []PushItem{deleteItem, movementDelete, reopened}})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete push status=%d body=%s", rec.Code, rec.Body.String())
	}
	var domainDeleted, journalDeleted *time.Time
	if err = db.Raw(`SELECT deleted_at FROM expenses WHERE gym_id=? AND id=?`, gymID, expenseID).Row().Scan(&domainDeleted); err != nil {
		t.Fatal(err)
	}
	if err = db.Raw(`SELECT deleted_at FROM sync_entities WHERE gym_id=? AND entity_type='expenses' AND entity_id=?`, gymID, expenseID).Row().Scan(&journalDeleted); err != nil {
		t.Fatal(err)
	}
	if domainDeleted == nil || journalDeleted == nil {
		t.Fatalf("domain tombstone=%v journal tombstone=%v", domainDeleted, journalDeleted)
	}
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

func performSyncPush(r http.Handler, token string, body PushRequest) (PushResponse, int, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return PushResponse{}, 0, err
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/push", bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var response PushResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		return PushResponse{}, rec.Code, err
	}
	return response, rec.Code, nil
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
	// must UPDATE the user row in place via ON CONFLICT. El payload incluye
	// password_hash="new-hash" a propósito: la guarda de credenciales
	// (guardUserCredential) debe DESCARTARLO para un owner — el cloud es la
	// autoridad del password del dashboard — y preservar el hash sembrado
	// ('unused'), mientras el resto del perfil sí se actualiza.
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
	// full_name y version SÍ se actualizan; password_hash del owner se PRESERVA
	// ('unused', el sembrado) porque la guarda lo descartó del push.
	if got.FullName != "Renamed Owner" || got.Version != 2 {
		t.Errorf("profile no actualizado por el projector: %+v", got)
	}
	if got.PasswordHash != "unused" {
		t.Errorf("password_hash del owner NO debe cambiar por un push (cloud es autoridad); got %q, want %q", got.PasswordHash, "unused")
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

// TestPullAugmentsGymWithCanonicalFields — el sidecar nunca emite los
// campos cloud-owned en su push (billing). Otros — created_at,
// kiosk_settings — sí los emite post-fix pero sidecars pre-fix los
// dejaban afuera del payload, y esos payloads quedaron guardados en
// sync_entities. Para que un sidecar full-syncing pueda hacer INSERT en
// su SQLite local (donde subscription_plan, subscription_status,
// created_at, kiosk_settings son TODOS NOT NULL), el pull cloud-side
// inyecta esos campos desde el row vivo de gyms — antes el payload se
// devolvía verbatim y la INSERT rompía con NOT NULL constraint failed.
//
// Setup: push de un gym con payload "sidecar pre-fix" (sin billing,
// sin created_at, sin kiosk_settings). Update directo de gyms con
// valores canónicos. Pull con since=epoch-cero → el payload devuelto
// debe traer TODOS los campos canónicos desde gyms.
func TestPullAugmentsGymWithCanonicalFields(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, _ := tokens.GenerateAccessToken(userID, gymID, "owner")

	// Push del gym con payload "sidecar pre-fix" — sin subscription_*,
	// sin created_at, sin kiosk_settings. Emula exactamente el shape
	// que sidecars antiguos guardaron en sync_entities.
	pushBody := map[string]any{
		"id":         gymID.String(),
		"gym_id":     gymID.String(),
		"version":    2,
		"name":       "Pull Augment Gym",
		"country":    "MX",
		"timezone":   "America/Mexico_City",
		"updated_at": time.Now().UnixMilli(),
	}
	pb, _ := json.Marshal(pushBody)
	pushReq := PushRequest{
		ClientID: uuid.NewString(), ClientNow: time.Now(), SchemaVersion: 1,
		Batch: []PushItem{{
			QueueID: uuid.NewString(), EntityType: "gyms", EntityID: gymID.String(),
			Operation: OpUpsertStr, ClientVersion: 2, Payload: pb, EnqueuedAt: time.Now(),
		}},
	}
	if rec := doJSONReq(t, r, "POST", "/api/v1/sync/push", tok, pushReq); rec.Code != 200 {
		t.Fatalf("push status %d: %s", rec.Code, rec.Body.String())
	}

	// Bump del gym row con valores canónicos. Simula:
	//   - billing: webhook de Stripe / RecordEvent.
	//   - created_at: vino del INSERT inicial via DEFAULT NOW().
	//   - kiosk_settings: operador cambió audio_volume desde el desktop.
	trialEnds := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	subEnds := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	createdAt := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := db.Exec(`
		UPDATE gyms
		   SET subscription_plan = 'plus_monthly',
		       subscription_status = 'active',
		       stripe_customer_id = 'cus_test_aug_123',
		       trial_ends_at = ?,
		       subscription_ends_at = ?,
		       created_at = ?,
		       kiosk_settings = '{"audio_volume":42,"auto_close_seconds":7}'::jsonb
		 WHERE id = ?`, trialEnds, subEnds, createdAt, gymID).Error; err != nil {
		t.Fatalf("update gym canonical: %v", err)
	}

	// Pull desde el principio del tiempo — debe devolver al menos el gym.
	rec := doJSONReq(t, r, "GET",
		"/api/v1/sync/pull?since="+url.QueryEscape("1970-01-01T00:00:00Z")+"&limit=500",
		tok, nil)
	if rec.Code != 200 {
		t.Fatalf("pull status %d: %s", rec.Code, rec.Body.String())
	}
	var resp PullResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode pull: %v", err)
	}

	var gymChange *PullChange
	for i := range resp.Changes {
		if resp.Changes[i].EntityType == "gyms" && resp.Changes[i].EntityID == gymID.String() {
			gymChange = &resp.Changes[i]
			break
		}
	}
	if gymChange == nil {
		t.Fatalf("pull no devolvió el gym: changes=%d", len(resp.Changes))
	}
	var pl map[string]any
	if err := json.Unmarshal(gymChange.Payload, &pl); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	if v, _ := pl["subscription_plan"].(string); v != "plus_monthly" {
		t.Errorf("subscription_plan = %v, want plus_monthly", pl["subscription_plan"])
	}
	if v, _ := pl["subscription_status"].(string); v != "active" {
		t.Errorf("subscription_status = %v, want active", pl["subscription_status"])
	}
	if v, _ := pl["stripe_customer_id"].(string); v != "cus_test_aug_123" {
		t.Errorf("stripe_customer_id = %v", pl["stripe_customer_id"])
	}
	// trial_ends_at debe venir como epoch ms (JSON number), no string ISO.
	te, ok := pl["trial_ends_at"].(float64)
	if !ok {
		t.Errorf("trial_ends_at no es number: %T (%v)", pl["trial_ends_at"], pl["trial_ends_at"])
	} else if want := trialEnds.UnixMilli(); int64(te) != want {
		t.Errorf("trial_ends_at = %d, want %d", int64(te), want)
	}
	se, ok := pl["subscription_ends_at"].(float64)
	if !ok {
		t.Errorf("subscription_ends_at no es number: %T", pl["subscription_ends_at"])
	} else if want := subEnds.UnixMilli(); int64(se) != want {
		t.Errorf("subscription_ends_at = %d, want %d", int64(se), want)
	}
	// created_at: aunque el payload pre-fix no lo emitió, el augment lo
	// inyecta desde gyms.created_at — es exactamente el bug del piloto.
	ca, ok := pl["created_at"].(float64)
	if !ok {
		t.Errorf("created_at no es number: %T (%v)", pl["created_at"], pl["created_at"])
	} else if want := createdAt.UnixMilli(); int64(ca) != want {
		t.Errorf("created_at = %d, want %d", int64(ca), want)
	}
	// kiosk_settings: igual — pre-fix lo omitía y el augment lo inyecta.
	ks, ok := pl["kiosk_settings"].(map[string]any)
	if !ok {
		t.Errorf("kiosk_settings no es objeto: %T (%v)", pl["kiosk_settings"], pl["kiosk_settings"])
	} else {
		if v, _ := ks["audio_volume"].(float64); int(v) != 42 {
			t.Errorf("kiosk_settings.audio_volume = %v, want 42", ks["audio_volume"])
		}
	}
	// Sanity: name del push viejo se preservó (no toda la fila se reemplazó).
	if v, _ := pl["name"].(string); v != "Pull Augment Gym" {
		t.Errorf("name = %v, want Pull Augment Gym", pl["name"])
	}
}

// Historical cloud writes stored membership type money in cents inside
// sync_entities, while the canonical wire is pesos. Both incremental pull and
// full-sync must overlay the live Postgres NUMERIC row so a sidecar does not
// materialise cents² (Normal $450 becoming $45,000).
func TestPullAndFullSyncAugmentMembershipTypeFromCanonicalRow(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, _ := tokens.GenerateAccessToken(userID, gymID, "owner")
	typeID := uuid.New()
	if err := db.Exec(`
		INSERT INTO membership_types
		    (id, gym_id, version, created_at, updated_at, name, price,
		     duration_days, duration_months, enrollment_fee,
		     maintenance_fee, maintenance_frequency, active)
		VALUES (?, ?, 1, NOW(), NOW(), 'Normal', 450, 30, 1, 0, 0, NULL, TRUE)`,
		typeID, gymID).Error; err != nil {
		t.Fatalf("seed canonical membership type: %v", err)
	}
	// Exact stale shape: monetary values were cents in the JSON journal.
	if err := db.Exec(`
		INSERT INTO sync_entities
		    (gym_id, entity_type, entity_id, version, payload, server_updated_at)
		VALUES (?, 'membership_types', ?, 1,
		        jsonb_build_object(
		            'id', ?::text, 'gym_id', ?::text, 'version', 1,
		            'name', 'Normal', 'price', 45000,
		            'duration_days', 30, 'duration_months', 1,
		            'enrollment_fee', 0, 'maintenance_fee', 0,
		            'maintenance_frequency', NULL, 'active', TRUE,
		            'created_at', 1, 'updated_at', 1
		        ), NOW())`, gymID, typeID, typeID, gymID).Error; err != nil {
		t.Fatalf("seed stale journal: %v", err)
	}

	assertCanonical := func(path string) {
		t.Helper()
		rec := doJSONReq(t, r, "GET", path, tok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status %d: %s", path, rec.Code, rec.Body.String())
		}
		var resp PullResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		for _, ch := range resp.Changes {
			if ch.EntityType != "membership_types" || ch.EntityID != typeID.String() {
				continue
			}
			var pl map[string]any
			if err := json.Unmarshal(ch.Payload, &pl); err != nil {
				t.Fatalf("decode membership type payload: %v", err)
			}
			if pl["price"] != float64(450) {
				t.Fatalf("GET %s price=%v, want canonical pesos 450", path, pl["price"])
			}
			return
		}
		t.Fatalf("GET %s did not return membership type %s", path, typeID)
	}
	assertCanonical("/api/v1/sync/pull?since=" + url.QueryEscape("1970-01-01T00:00:00Z") + "&limit=500")
	assertCanonical("/api/v1/sync/full?limit=5000")
}

// Historical imports also left stock_movements.cost in cents inside the
// journal while the live Postgres row correctly stored pesos. Both pull paths
// must send the canonical $5, never the stale 500 that SQLite would multiply
// into 50,000 cents ($500).
func TestPullAndFullSyncAugmentStockMovementCostFromCanonicalRow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		storedCost any
		wireCost   any
	}{
		{"positive", 5, float64(5)},
		{"unknown", nil, nil},
		{"legacy_zero", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := projectorTestDB(t)
			gymID, userID := seedGymAndOwner(t, db)
			r, tokens := newRealHandler(t, db)
			tok, _ := tokens.GenerateAccessToken(userID, gymID, "owner")
			productID := uuid.New()
			movementID := uuid.New()

			if err := db.Exec(`
		INSERT INTO products
		    (id, gym_id, version, created_at, updated_at, name, price, stock, stock_minimum, active)
		VALUES (?, ?, 1, NOW(), NOW(), 'Agua de prueba', 15, 24, 2, TRUE)`,
				productID, gymID).Error; err != nil {
				t.Fatalf("seed product: %v", err)
			}
			// Reproduce a row predating migration 039, then restore the exact
			// NOT VALID constraint before either HTTP request. DDL is transactional:
			// any fixture failure rolls back both the row and the schema change.
			if err := db.Transaction(func(tx *gorm.DB) error {
				if tc.name == "legacy_zero" {
					if err := tx.Exec(`ALTER TABLE stock_movements DROP CONSTRAINT chk_stock_movements_positive_cost`).Error; err != nil {
						return err
					}
				}
				if err := tx.Exec(`
		INSERT INTO stock_movements
		    (id, gym_id, version, created_at, updated_at, product_id,
		     movement_type, delta, reason, cost, is_purchase, operator_id)
		VALUES (?, ?, 1, NOW(), NOW(), ?, 'restock', 24,
		        'Inventario inicial', ?, FALSE, ?)`,
					movementID, gymID, productID, tc.storedCost, userID).Error; err != nil {
					return err
				}
				if tc.name == "legacy_zero" {
					return tx.Exec(`ALTER TABLE stock_movements ADD CONSTRAINT chk_stock_movements_positive_cost CHECK (cost IS NULL OR cost > 0) NOT VALID`).Error
				}
				return nil
			}); err != nil {
				t.Fatalf("seed canonical stock movement: %v", err)
			}
			if err := db.Exec(`
		INSERT INTO sync_entities
		    (gym_id, entity_type, entity_id, version, payload, server_updated_at)
		VALUES (?, 'stock_movements', ?, 1,
		        jsonb_build_object(
		            'id', ?::text, 'gym_id', ?::text, 'version', 1,
		            'product_id', ?::text, 'movement_type', 'restock',
		            'delta', 24, 'reason', 'Inventario inicial',
		            'cost', 500, 'is_purchase', FALSE,
		            'sale_item_id', NULL, 'operator_id', ?::text,
		            'created_at', 1, 'updated_at', 1
		        ), NOW())`,
				gymID, movementID, movementID, gymID, productID, userID).Error; err != nil {
				t.Fatalf("seed stale stock movement journal: %v", err)
			}

			assertCanonical := func(path string) {
				t.Helper()
				rec := doJSONReq(t, r, "GET", path, tok, nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s status %d: %s", path, rec.Code, rec.Body.String())
				}
				var resp PullResponse
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("decode %s: %v", path, err)
				}
				for _, ch := range resp.Changes {
					if ch.EntityType != "stock_movements" || ch.EntityID != movementID.String() {
						continue
					}
					var pl map[string]any
					if err := json.Unmarshal(ch.Payload, &pl); err != nil {
						t.Fatalf("decode stock movement payload: %v", err)
					}
					if cost, exists := pl["cost"]; !exists || cost != tc.wireCost {
						t.Fatalf("GET %s cost=%v (present=%v), want %v", path, cost, exists, tc.wireCost)
					}
					if pl["is_purchase"] != false {
						t.Fatalf("GET %s is_purchase=%v, want false", path, pl["is_purchase"])
					}
					return
				}
				t.Fatalf("GET %s did not return stock movement %s", path, movementID)
			}

			assertCanonical("/api/v1/sync/pull?since=" + url.QueryEscape("1970-01-01T00:00:00Z") + "&limit=500")
			assertCanonical("/api/v1/sync/full?limit=5000")
		})
	}
}

// TestPullAugmentationLeavesNonGymRowsUntouched — salvo los tipos con
// overlay canónico explícito (gyms, membership_types, stock_movements), otras entidades
// como members/payments vienen verbatim. Si el CASE estuviera mal escrito
// (ej. olvidamos el ELSE), aparecerían con campos extras del JOIN y la
// INSERT del sidecar rompería con "no such column".
func TestPullAugmentationLeavesNonGymRowsUntouched(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, _ := tokens.GenerateAccessToken(userID, gymID, "owner")

	// Push de un member para tener al menos una fila non-gym en sync_entities.
	memberID := uuid.New()
	mb, _ := json.Marshal(map[string]any{
		"id": memberID.String(), "gym_id": gymID.String(), "version": 1,
		"folio": "M-AUG-001", "full_name": "Aug Member", "phone": "555", "status": "active",
		"enrollment_paid": false, "created_by": userID.String(),
		"updated_at": time.Now().UnixMilli(),
	})
	pushReq := PushRequest{
		ClientID: uuid.NewString(), ClientNow: time.Now(), SchemaVersion: 1,
		Batch: []PushItem{{
			QueueID: uuid.NewString(), EntityType: "members", EntityID: memberID.String(),
			Operation: OpUpsertStr, ClientVersion: 1, Payload: mb, EnqueuedAt: time.Now(),
		}},
	}
	if rec := doJSONReq(t, r, "POST", "/api/v1/sync/push", tok, pushReq); rec.Code != 200 {
		t.Fatalf("push status %d: %s", rec.Code, rec.Body.String())
	}

	rec := doJSONReq(t, r, "GET",
		"/api/v1/sync/pull?since="+url.QueryEscape("1970-01-01T00:00:00Z")+"&limit=500",
		tok, nil)
	if rec.Code != 200 {
		t.Fatalf("pull status %d: %s", rec.Code, rec.Body.String())
	}
	var resp PullResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode pull: %v", err)
	}

	var found *PullChange
	for i := range resp.Changes {
		if resp.Changes[i].EntityType == "members" && resp.Changes[i].EntityID == memberID.String() {
			found = &resp.Changes[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("member pull no devuelto")
	}
	var pl map[string]any
	if err := json.Unmarshal(found.Payload, &pl); err != nil {
		t.Fatalf("decode member payload: %v", err)
	}
	for _, leak := range []string{
		"subscription_plan", "subscription_status", "stripe_customer_id",
		"trial_ends_at", "subscription_ends_at", "kiosk_settings",
	} {
		if _, ok := pl[leak]; ok {
			t.Errorf("non-gym payload contaminated con %s: %v", leak, pl[leak])
		}
	}
	if v, _ := pl["folio"].(string); v != "M-AUG-001" {
		t.Errorf("member payload se perdió: folio=%v", pl["folio"])
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

// Two offline desks may both validate against the same stale local snapshot.
// Their rows have different UUIDs, so ordinary row-level LWW cannot protect
// the aggregate. The cloud must accept at most one monetary claim/revision and
// return a permanent, legible conflict for the other one.
func TestTwoSidecars_FinancialAggregatesRejectLosingRefundAndCorrection(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)

	rootID, refundPaymentA, refundPaymentB := uuid.New(), uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,parent_payment_id,discount_amount,balance_pending,
		payment_date,operator_id)
		VALUES
		 (?, ?, 1, ?, ?, 'OTH-SYNC-ROOT', 100, 100, 'transfer', 'other', NULL, 0, 0, ?, ?),
		 (?, ?, 1, ?, ?, 'RFD-SYNC-A', -60, 0, 'transfer', 'refund', ?, 0, 0, ?, ?),
		 (?, ?, 1, ?, ?, 'RFD-SYNC-B', -60, 0, 'transfer', 'refund', ?, 0, 0, ?, ?)`,
		rootID, gymID, now, now, now, userID,
		refundPaymentA, gymID, now, now, rootID, now, userID,
		refundPaymentB, gymID, now, now, rootID, now, userID).Error; err != nil {
		t.Fatalf("seed refund payments: %v", err)
	}

	pushOne := func(clientID uuid.UUID, entityType string, entityID uuid.UUID, payload map[string]any) PushItemResult {
		t.Helper()
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		rec := doJSONReq(t, router, http.MethodPost, "/api/v1/sync/push", token, PushRequest{
			ClientID: clientID.String(), ClientNow: now, SchemaVersion: SchemaVersion,
			Batch: []PushItem{{
				QueueID: uuid.NewString(), EntityType: entityType, EntityID: entityID.String(),
				Operation: OpUpsertStr, ClientVersion: 1, Payload: encoded, EnqueuedAt: now,
			}},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("push %s status=%d body=%s", entityType, rec.Code, rec.Body.String())
		}
		var response PushResponse
		if decodeErr := json.Unmarshal(rec.Body.Bytes(), &response); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if len(response.Results) != 1 {
			t.Fatalf("push %s results=%+v", entityType, response.Results)
		}
		return response.Results[0]
	}

	refundPayload := func(id, refundPaymentID uuid.UUID, key string) map[string]any {
		return map[string]any{
			"id": id.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"root_payment_id": rootID.String(), "refund_payment_id": refundPaymentID.String(),
			"sale_id": nil, "amount": 60.0, "method": "transfer", "refunded_on": now.Format("2006-01-02"),
			"reason": "Devolución offline", "kind": "revenue_refund", "balance_cancelled": 0,
			"legacy_incomplete": false, "correction_id": nil,
			"idempotency_key": key, "idempotency_fingerprint": "fp:" + key,
			"idempotency_result": map[string]any{"amount": 60}, "created_by": userID.String(),
		}
	}
	clientA, clientB := uuid.New(), uuid.New()
	refundA, refundB := uuid.New(), uuid.New()
	if result := pushOne(clientA, "refunds", refundA, refundPayload(refundA, refundPaymentA, "refund:desk-a")); result.Status != StatusAccepted {
		t.Fatalf("first refund=%+v", result)
	}
	losingRefund := pushOne(clientB, "refunds", refundB, refundPayload(refundB, refundPaymentB, "refund:desk-b"))
	if losingRefund.Status != StatusRejectedFinancialConflict || !strings.Contains(losingRefund.Error, "quedan $40.00") {
		t.Fatalf("second refund=%+v, want financial conflict with remaining balance", losingRefund)
	}
	var refundCount int64
	var refundedCents int64
	if err := db.Raw(`SELECT COUNT(*) AS count,
		COALESCE(SUM(ROUND(amount*100)::bigint),0)::bigint AS cents
		FROM refunds WHERE gym_id=? AND root_payment_id=? AND deleted_at IS NULL`, gymID, rootID).
		Row().Scan(&refundCount, &refundedCents); err != nil {
		t.Fatal(err)
	}
	if refundCount != 1 || refundedCents != 6000 {
		t.Fatalf("canonical refunds count=%d cents=%d", refundCount, refundedCents)
	}

	// Seed the already-mutated Sale row that each sidecar would enqueue before
	// its immutable correction audit row. Both started from revision zero.
	salePaymentID, saleID := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 2, ?, ?, 'PRD-SYNC-CORR', 40, 40, 'transfer', 'product', 10, 0, ?, ?)`,
		salePaymentID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatalf("seed correction payment: %v", err)
	}
	if err := db.Exec(`INSERT INTO sales(
		id,gym_id,version,created_at,updated_at,payment_id,subtotal,discount,total,correction_version)
		VALUES (?, ?, 2, ?, ?, ?, 50, 10, 40, 1)`,
		saleID, gymID, now, now, salePaymentID).Error; err != nil {
		t.Fatalf("seed corrected sale: %v", err)
	}
	correctionPayload := func(id uuid.UUID, key string) map[string]any {
		return map[string]any{
			"id": id.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"sale_id": saleID.String(), "expected_sale_version": 0,
			"reason": "Cantidad capturada mal", "money_resolution": "record_only", "monetary_delta": 10.0,
			"before_snapshot": map[string]any{
				"sale_id": saleID.String(), "correction_version": 0,
				"subtotal_cents": 5000, "discount_cents": 0, "total_cents": 5000,
			},
			"after_snapshot": map[string]any{
				"sale_id": saleID.String(), "correction_version": 1,
				"subtotal_cents": 5000, "discount_cents": 1000, "total_cents": 4000,
			},
			"idempotency_key": key, "idempotency_fingerprint": "fp:" + key,
			"idempotency_result": map[string]any{"correction_id": id.String()}, "created_by": userID.String(),
		}
	}
	correctionA, correctionB := uuid.New(), uuid.New()
	if result := pushOne(clientA, "sale_corrections", correctionA, correctionPayload(correctionA, "correction:desk-a")); result.Status != StatusAccepted {
		t.Fatalf("first correction=%+v", result)
	}
	losingCorrection := pushOne(clientB, "sale_corrections", correctionB, correctionPayload(correctionB, "correction:desk-b"))
	if losingCorrection.Status != StatusRejectedFinancialConflict || !strings.Contains(losingCorrection.Error, "otro escritorio") {
		t.Fatalf("second correction=%+v, want competing revision conflict", losingCorrection)
	}
	var correctionCount int64
	if err := db.Table("sale_corrections").Where("gym_id=? AND sale_id=? AND deleted_at IS NULL", gymID, saleID).Count(&correctionCount).Error; err != nil {
		t.Fatal(err)
	}
	if correctionCount != 1 {
		t.Fatalf("canonical corrections=%d, want 1", correctionCount)
	}
}

// A real offline refund reaches sync as a small graph: the negative Payment
// is queued before its immutable Refund aggregate. The ledger half must be
// guarded too; otherwise rejecting only the Refund would still leave reports
// with two negative payments. Both HTTP requests start together and use
// different client IDs, matching two independent sidecars.
func TestTwoSidecars_ConcurrentRefundGraphsCannotOverRefundLedger(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	rootID := uuid.New()
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 1, ?, ?, 'OTH-SYNC-CONCURRENT', 100, 100, 'transfer', 'other', 0, 0, ?, ?)`,
		rootID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatalf("seed root payment: %v", err)
	}

	makeItem := func(entityType string, entityID uuid.UUID, payload map[string]any) PushItem {
		t.Helper()
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: entityType, EntityID: entityID.String(),
			Operation: OpUpsertStr, ClientVersion: 1, Payload: encoded, EnqueuedAt: now}
	}
	makeRequest := func(label string) PushRequest {
		refundPaymentID, refundID := uuid.New(), uuid.New()
		payment := map[string]any{
			"id": refundPaymentID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"folio": "RFD-SYNC-" + label, "member_id": nil, "membership_id": nil,
			"amount": -60.0, "recognized_amount": 0, "payment_method": "transfer",
			"concept": "refund", "parent_payment_id": rootID.String(), "discount_amount": 0,
			"balance_pending": 0, "payment_date": now.Format("2006-01-02"),
			"operator_id": userID.String(),
		}
		refund := map[string]any{
			"id": refundID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"root_payment_id": rootID.String(), "refund_payment_id": refundPaymentID.String(),
			"sale_id": nil, "amount": 60.0, "method": "transfer", "refunded_on": now.Format("2006-01-02"),
			"reason": "Devolución simultánea " + label, "kind": "revenue_refund", "balance_cancelled": 0,
			"legacy_incomplete": false, "correction_id": nil,
			"idempotency_key":         "refund:concurrent:" + label,
			"idempotency_fingerprint": "fingerprint:" + label,
			"idempotency_result":      map[string]any{"refund_id": refundID.String(), "amount": 60},
			"created_by":              userID.String(),
		}
		return PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion,
			Batch: []PushItem{makeItem("payments", refundPaymentID, payment), makeItem("refunds", refundID, refund)}}
	}
	requests := []PushRequest{makeRequest("A"), makeRequest("B")}
	type outcome struct {
		response PushResponse
		status   int
		err      error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(requests))
	for _, request := range requests {
		request := request
		go func() {
			<-start
			response, status, pushErr := performSyncPush(router, token, request)
			outcomes <- outcome{response: response, status: status, err: pushErr}
		}()
	}
	close(start)

	acceptedGraphs, rejectedGraphs := 0, 0
	for range requests {
		out := <-outcomes
		if out.err != nil || out.status != http.StatusOK || len(out.response.Results) != 2 {
			t.Fatalf("concurrent push status=%d err=%v response=%+v", out.status, out.err, out.response)
		}
		statuses := []string{out.response.Results[0].Status, out.response.Results[1].Status}
		switch {
		case statuses[0] == StatusAccepted && statuses[1] == StatusAccepted:
			acceptedGraphs++
		case statuses[0] == StatusRejectedFinancialConflict && statuses[1] == StatusRejectedFinancialConflict:
			rejectedGraphs++
		default:
			t.Fatalf("refund graph did not converge atomically enough: statuses=%v results=%+v", statuses, out.response.Results)
		}
	}
	if acceptedGraphs != 1 || rejectedGraphs != 1 {
		t.Fatalf("accepted graphs=%d rejected graphs=%d, want 1/1", acceptedGraphs, rejectedGraphs)
	}

	var ledger struct {
		Rows  int64 `gorm:"column:rows"`
		Cents int64 `gorm:"column:cents"`
	}
	if err := db.Raw(`SELECT COUNT(*) AS rows,
		COALESCE(SUM(ROUND(ABS(amount)*100)::bigint),0)::bigint AS cents
		FROM payments WHERE gym_id=? AND concept='refund' AND parent_payment_id=? AND deleted_at IS NULL`,
		gymID, rootID).Scan(&ledger).Error; err != nil {
		t.Fatal(err)
	}
	var refundRows int64
	if err := db.Table("refunds").
		Where("gym_id=? AND root_payment_id=? AND deleted_at IS NULL", gymID, rootID).Count(&refundRows).Error; err != nil {
		t.Fatal(err)
	}
	if ledger.Rows != 1 || ledger.Cents != 6000 || refundRows != 1 {
		t.Fatalf("canonical ledger rows=%d cents=%d refunds=%d, want 1/$60/1", ledger.Rows, ledger.Cents, refundRows)
	}
}

// Both desks still see $100 globally refundable, and each returns the same
// $50 product. Monetary capacity alone would accept both negative Payments;
// the item quantity is the invariant that must roll the losing graph back.
func TestTwoSidecars_SameProductRefundRollsBackEntireLosingGraph(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	rootID, saleID := uuid.New(), uuid.New()
	targetProductID, otherProductID := uuid.New(), uuid.New()
	targetLineID, otherLineID := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 1, ?, ?, 'PRD-SYNC-ITEM-RACE', 100, 100, 'transfer', 'product', 0, 0, ?, ?)`,
		rootID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sales(
		id,gym_id,version,created_at,updated_at,payment_id,subtotal,discount,total,correction_version)
		VALUES (?, ?, 1, ?, ?, ?, 100, 0, 100, 0)`, saleID, gymID, now, now, rootID).Error; err != nil {
		t.Fatal(err)
	}
	for _, product := range []struct {
		id   uuid.UUID
		name string
	}{{targetProductID, "Producto simultáneo"}, {otherProductID, "Producto restante"}} {
		if err := db.Exec(`INSERT INTO products(
			id,gym_id,version,created_at,updated_at,name,price,stock,stock_minimum,active)
			VALUES (?, ?, 1, ?, ?, ?, 50, 1, 0, TRUE)`,
			product.id, gymID, now, now, product.name).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, line := range []struct {
		id, productID uuid.UUID
		name          string
	}{{targetLineID, targetProductID, "Producto simultáneo"}, {otherLineID, otherProductID, "Producto restante"}} {
		if err := db.Exec(`INSERT INTO sale_items(
			id,gym_id,version,created_at,updated_at,sale_id,product_id,product_name_snapshot,
			unit_price_snapshot,unit_cost_snapshot,quantity,line_total)
			VALUES (?, ?, 1, ?, ?, ?, ?, ?, 50, 25, 1, 50)`,
			line.id, gymID, now, now, saleID, line.productID, line.name).Error; err != nil {
			t.Fatal(err)
		}
	}

	makeItem := func(entityType string, entityID uuid.UUID, version int, payload map[string]any) PushItem {
		t.Helper()
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: entityType, EntityID: entityID.String(),
			Operation: OpUpsertStr, ClientVersion: version, Payload: encoded, EnqueuedAt: now}
	}
	type graph struct {
		request       PushRequest
		refundID      uuid.UUID
		paymentID     uuid.UUID
		refundItemID  uuid.UUID
		refundItemRaw map[string]any
	}
	makeGraph := func(label string) graph {
		refundID, paymentID, itemID := uuid.New(), uuid.New(), uuid.New()
		payment := map[string]any{
			"id": paymentID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"folio": "RFD-ITEM-RACE-" + label, "member_id": nil, "membership_id": nil,
			"amount": -50.0, "recognized_amount": 0, "payment_method": "transfer",
			"concept": "refund", "parent_payment_id": rootID.String(), "discount_amount": 0,
			"balance_pending": 0, "payment_date": now.Format("2006-01-02"), "operator_id": userID.String(),
		}
		refund := map[string]any{
			"id": refundID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"root_payment_id": rootID.String(), "refund_payment_id": paymentID.String(), "sale_id": saleID.String(),
			"amount": 50.0, "method": "transfer", "refunded_on": now.Format("2006-01-02"),
			"reason": "Misma unidad desde recepción " + label, "kind": "revenue_refund", "balance_cancelled": 0,
			"legacy_incomplete": false, "correction_id": nil,
			"idempotency_key": "refund:item-race:" + label, "idempotency_fingerprint": "fingerprint:item-race:" + label,
			"idempotency_result": map[string]any{"refund_id": refundID.String(), "amount": 50}, "created_by": userID.String(),
		}
		refundItem := map[string]any{
			"id": itemID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"refund_id": refundID.String(), "sale_item_id": targetLineID.String(),
			"quantity": 1, "amount": 50.0, "disposition": "not_returned",
		}
		return graph{refundID: refundID, paymentID: paymentID, refundItemID: itemID, refundItemRaw: refundItem,
			request: PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion,
				Batch: []PushItem{
					makeItem("payments", paymentID, 1, payment),
					makeItem("refunds", refundID, 1, refund),
					makeItem("refund_items", itemID, 1, refundItem),
				}}}
	}
	graphs := []graph{makeGraph("A"), makeGraph("B")}
	type outcome struct {
		index    int
		response PushResponse
		status   int
		err      error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(graphs))
	for index, candidate := range graphs {
		index, request := index, candidate.request
		go func() {
			<-start
			response, status, pushErr := performSyncPush(router, token, request)
			outcomes <- outcome{index: index, response: response, status: status, err: pushErr}
		}()
	}
	close(start)
	winner, loser := -1, -1
	for range graphs {
		out := <-outcomes
		if out.err != nil || out.status != http.StatusOK || len(out.response.Results) != 3 {
			t.Fatalf("same-item graph status=%d err=%v response=%+v", out.status, out.err, out.response)
		}
		statuses := []string{out.response.Results[0].Status, out.response.Results[1].Status, out.response.Results[2].Status}
		switch {
		case statuses[0] == StatusAccepted && statuses[1] == StatusAccepted && statuses[2] == StatusAccepted:
			winner = out.index
		case statuses[0] == StatusRejectedFinancialConflict &&
			statuses[1] == StatusRejectedFinancialConflict && statuses[2] == StatusRejectedFinancialConflict:
			loser = out.index
		default:
			t.Fatalf("same-item graph committed partially: %+v", out.response.Results)
		}
	}
	if winner < 0 || loser < 0 || winner == loser {
		t.Fatalf("winner=%d loser=%d", winner, loser)
	}
	var canonical struct {
		Payments int64 `gorm:"column:payments"`
		Refunds  int64 `gorm:"column:refunds"`
		Items    int64 `gorm:"column:items"`
		Cents    int64 `gorm:"column:cents"`
	}
	if err := db.Raw(`SELECT
		(SELECT COUNT(*) FROM payments WHERE gym_id=? AND concept='refund' AND parent_payment_id=? AND deleted_at IS NULL) AS payments,
		(SELECT COUNT(*) FROM refunds WHERE gym_id=? AND root_payment_id=? AND deleted_at IS NULL) AS refunds,
		(SELECT COUNT(*) FROM refund_items ri JOIN refunds r ON r.gym_id=ri.gym_id AND r.id=ri.refund_id
		 WHERE ri.gym_id=? AND r.root_payment_id=? AND ri.deleted_at IS NULL AND r.deleted_at IS NULL) AS items,
		(SELECT COALESCE(SUM(ROUND(ABS(amount)*100)::bigint),0) FROM payments
		 WHERE gym_id=? AND concept='refund' AND parent_payment_id=? AND deleted_at IS NULL) AS cents`,
		gymID, rootID, gymID, rootID, gymID, rootID, gymID, rootID).Scan(&canonical).Error; err != nil {
		t.Fatal(err)
	}
	if canonical.Payments != 1 || canonical.Refunds != 1 || canonical.Items != 1 || canonical.Cents != 5000 {
		t.Fatalf("canonical graph after race=%+v, want one complete $50 graph", canonical)
	}
	for _, id := range []uuid.UUID{graphs[loser].paymentID, graphs[loser].refundID, graphs[loser].refundItemID} {
		var rows int64
		if err := db.Raw(`SELECT
			(SELECT COUNT(*) FROM payments WHERE id=?) +
			(SELECT COUNT(*) FROM refunds WHERE id=?) +
			(SELECT COUNT(*) FROM refund_items WHERE id=?) +
			(SELECT COUNT(*) FROM sync_entities WHERE entity_id=?)`, id, id, id, id).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("losing graph entity %s left %d canonical/journal rows", id, rows)
		}
	}

	// Lost HTTP responses are harmless: replaying the committed graph is a
	// semantic success and does not duplicate any financial row.
	replay, status, err := performSyncPush(router, token, graphs[winner].request)
	if err != nil || status != http.StatusOK || len(replay.Results) != 3 {
		t.Fatalf("replay status=%d err=%v response=%+v", status, err, replay)
	}
	for _, result := range replay.Results {
		if result.Status != StatusAccepted && result.Status != StatusConflictServerWins && result.Status != StatusConflictClientWins {
			t.Fatalf("idempotent replay result=%+v", result)
		}
	}

	// Immutable evidence cannot be deleted or rewritten with a larger LWW
	// version. The canonical line and complete graph survive both attempts.
	deleted := clonePayloadMap(graphs[winner].refundItemRaw)
	deleted["version"], deleted["updated_at"], deleted["deleted_at"] = 2, now.Add(time.Second).UnixMilli(), now.Add(time.Second).UnixMilli()
	deleteRequest := PushRequest{ClientID: uuid.NewString(), ClientNow: now.Add(time.Second), SchemaVersion: SchemaVersion,
		Batch: []PushItem{makeItem("refund_items", graphs[winner].refundItemID, 2, deleted)}}
	deleteResponse, status, err := performSyncPush(router, token, deleteRequest)
	if err != nil || status != http.StatusOK || len(deleteResponse.Results) != 1 ||
		deleteResponse.Results[0].Status != StatusRejectedFinancialConflict {
		t.Fatalf("refund item resurrection/delete status=%d err=%v response=%+v", status, err, deleteResponse)
	}
	rewritten := clonePayloadMap(graphs[winner].refundItemRaw)
	rewritten["version"], rewritten["updated_at"], rewritten["quantity"] = 3, now.Add(2*time.Second).UnixMilli(), 2
	rewriteRequest := PushRequest{ClientID: uuid.NewString(), ClientNow: now.Add(2 * time.Second), SchemaVersion: SchemaVersion,
		Batch: []PushItem{makeItem("refund_items", graphs[winner].refundItemID, 3, rewritten)}}
	rewriteResponse, status, err := performSyncPush(router, token, rewriteRequest)
	if err != nil || status != http.StatusOK || len(rewriteResponse.Results) != 1 ||
		rewriteResponse.Results[0].Status != StatusRejectedFinancialConflict {
		t.Fatalf("refund item rewrite status=%d err=%v response=%+v", status, err, rewriteResponse)
	}
}

func TestRefundPayment_IsInvisibleUntilCompleteGraphArrives(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	rootID, refundPaymentID, refundID := uuid.New(), uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 1, ?, ?, 'OTH-SYNC-STAGED', 100, 100, 'transfer', 'other', 0, 0, ?, ?)`,
		rootID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatal(err)
	}
	encode := func(entityType string, id uuid.UUID, payload map[string]any) PushItem {
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: entityType, EntityID: id.String(),
			Operation: OpUpsertStr, ClientVersion: 1, Payload: encoded, EnqueuedAt: now}
	}
	payment := map[string]any{
		"id": refundPaymentID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
		"folio": "RFD-SYNC-STAGED", "member_id": nil, "membership_id": nil,
		"amount": -40.0, "recognized_amount": 0, "payment_method": "transfer", "concept": "refund",
		"parent_payment_id": rootID.String(), "discount_amount": 0, "balance_pending": 0,
		"payment_date": now.Format("2006-01-02"), "operator_id": userID.String(),
	}
	incompleteRequest := PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion,
		Batch: []PushItem{encode("payments", refundPaymentID, payment)}}
	incomplete, status, err := performSyncPush(router, token, incompleteRequest)
	if err != nil || status != http.StatusOK || len(incomplete.Results) != 1 ||
		incomplete.Results[0].Status != StatusRejectedFinancialConflict {
		t.Fatalf("incomplete refund payment status=%d err=%v response=%+v", status, err, incomplete)
	}
	var partialRows int64
	if err := db.Raw(`SELECT
		(SELECT COUNT(*) FROM payments WHERE id=?) +
		(SELECT COUNT(*) FROM sync_entities WHERE entity_type='payments' AND entity_id=?)`,
		refundPaymentID, refundPaymentID).Scan(&partialRows).Error; err != nil {
		t.Fatal(err)
	}
	if partialRows != 0 {
		t.Fatalf("incomplete refund payment became visible in %d rows", partialRows)
	}

	refund := map[string]any{
		"id": refundID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
		"root_payment_id": rootID.String(), "refund_payment_id": refundPaymentID.String(), "sale_id": nil,
		"amount": 40.0, "method": "transfer", "refunded_on": now.Format("2006-01-02"),
		"reason": "Grafo completo después del reintento", "kind": "revenue_refund", "balance_cancelled": 0,
		"legacy_incomplete": false, "correction_id": nil,
		"idempotency_key": "refund:staged-retry", "idempotency_fingerprint": "fingerprint:staged-retry",
		"idempotency_result": map[string]any{"refund_id": refundID.String(), "amount": 40}, "created_by": userID.String(),
	}
	completeRequest := PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion,
		Batch: []PushItem{encode("payments", refundPaymentID, payment), encode("refunds", refundID, refund)}}
	complete, status, err := performSyncPush(router, token, completeRequest)
	if err != nil || status != http.StatusOK || len(complete.Results) != 2 ||
		complete.Results[0].Status != StatusAccepted || complete.Results[1].Status != StatusAccepted {
		t.Fatalf("complete refund retry status=%d err=%v response=%+v", status, err, complete)
	}
}

func TestRefundGraph_RejectsUnrelatedRowsAndRootMoneyRewrite(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	rootID, unrelatedID := uuid.New(), uuid.New()
	for _, seed := range []struct {
		id     uuid.UUID
		folio  string
		amount int
	}{{rootID, "OTH-GRAPH-ROOT", 100}, {unrelatedID, "OTH-GRAPH-UNRELATED", 200}} {
		if err := db.Exec(`INSERT INTO payments(
			id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
			payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
			VALUES (?, ?, 1, ?, ?, ?, ?, ?, 'transfer', 'other', 0, 0, ?, ?)`,
			seed.id, gymID, now, now, seed.folio, seed.amount, seed.amount, now, userID).Error; err != nil {
			t.Fatal(err)
		}
	}

	paymentPayload := func(id uuid.UUID, folio string, amount float64) map[string]any {
		return map[string]any{
			"id": id.String(), "gym_id": gymID.String(), "version": 2,
			"created_at": now.UnixMilli(), "updated_at": now.Add(time.Second).UnixMilli(), "deleted_at": nil,
			"folio": folio, "member_id": nil, "membership_id": nil, "cash_drawer_id": nil,
			"amount": amount, "recognized_amount": amount, "payment_method": "transfer", "concept": "other",
			"parent_payment_id": nil, "discount_amount": 0, "balance_pending": 0,
			"payment_date": now.Format("2006-01-02"), "operator_id": userID.String(),
		}
	}
	pushRejectedGraph := func(label string, positiveRows ...PushItem) (uuid.UUID, uuid.UUID) {
		t.Helper()
		refundPaymentID, refundID := uuid.New(), uuid.New()
		graphIDs := []string{refundID.String()}
		encode := func(entityType string, id uuid.UUID, payload map[string]any) PushItem {
			payload[refundGraphIDsKey] = graphIDs
			encoded, marshalErr := json.Marshal(payload)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			version, _ := numericInt(payload["version"])
			return PushItem{QueueID: uuid.NewString(), EntityType: entityType, EntityID: id.String(),
				Operation: OpUpsertStr, ClientVersion: version, Payload: encoded, EnqueuedAt: now}
		}
		negative := map[string]any{
			"id": refundPaymentID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"folio": "RFD-GRAPH-GUARD-" + label, "member_id": nil, "membership_id": nil, "cash_drawer_id": nil,
			"amount": -40.0, "recognized_amount": 0, "payment_method": "transfer", "concept": "refund",
			"parent_payment_id": rootID.String(), "discount_amount": 0, "balance_pending": 0,
			"payment_date": now.Format("2006-01-02"), "operator_id": userID.String(),
		}
		refund := map[string]any{
			"id": refundID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"root_payment_id": rootID.String(), "refund_payment_id": refundPaymentID.String(), "sale_id": nil,
			"amount": 40.0, "method": "transfer", "refunded_on": now.Format("2006-01-02"),
			"reason": "Validar cierre del grafo " + label, "kind": "revenue_refund", "balance_cancelled": 0,
			"legacy_incomplete": false, "correction_id": nil,
			"idempotency_key":         "refund:graph-guard:" + label,
			"idempotency_fingerprint": "fingerprint:graph-guard:" + label,
			"idempotency_result":      map[string]any{"refund_id": refundID.String()}, "created_by": userID.String(),
			refundGraphExpectedItemsKey: 0,
		}
		batch := append([]PushItem(nil), positiveRows...)
		for index := range batch {
			var raw map[string]any
			if marshalErr := json.Unmarshal(batch[index].Payload, &raw); marshalErr != nil {
				t.Fatal(marshalErr)
			}
			raw[refundGraphIDsKey] = graphIDs
			batch[index].Payload, _ = json.Marshal(raw)
		}
		batch = append(batch, encode("payments", refundPaymentID, negative), encode("refunds", refundID, refund))
		response, status, pushErr := performSyncPush(router, token, PushRequest{
			ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion, Batch: batch,
		})
		if pushErr != nil || status != http.StatusOK || len(response.Results) != len(batch) {
			t.Fatalf("guard graph %s status=%d err=%v response=%+v", label, status, pushErr, response)
		}
		for _, result := range response.Results {
			if result.Status != StatusRejectedFinancialConflict {
				t.Fatalf("guard graph %s row=%+v", label, result)
			}
		}
		return refundPaymentID, refundID
	}
	makePositiveItem := func(id uuid.UUID, payload map[string]any) PushItem {
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: "payments", EntityID: id.String(),
			Operation: OpUpsertStr, ClientVersion: 2, Payload: encoded, EnqueuedAt: now}
	}

	rootRewritePayment, rootRewriteRefund := pushRejectedGraph("root-rewrite",
		makePositiveItem(rootID, paymentPayload(rootID, "OTH-GRAPH-ROOT", 1000)))
	unrelatedPayment, unrelatedRefund := pushRejectedGraph("unrelated-row",
		makePositiveItem(rootID, paymentPayload(rootID, "OTH-GRAPH-ROOT", 100)),
		makePositiveItem(unrelatedID, paymentPayload(unrelatedID, "OTH-GRAPH-UNRELATED", 999)))

	var canonical struct {
		RootCents      int64 `gorm:"column:root_cents"`
		UnrelatedCents int64 `gorm:"column:unrelated_cents"`
		PartialRows    int64 `gorm:"column:partial_rows"`
	}
	if err := db.Raw(`SELECT
		(SELECT ROUND(amount*100)::bigint FROM payments WHERE gym_id=? AND id=?) AS root_cents,
		(SELECT ROUND(amount*100)::bigint FROM payments WHERE gym_id=? AND id=?) AS unrelated_cents,
		((SELECT COUNT(*) FROM payments WHERE id IN (?,?)) +
		 (SELECT COUNT(*) FROM refunds WHERE id IN (?,?)) +
		 (SELECT COUNT(*) FROM sync_entities WHERE entity_id IN (?,?,?,?))) AS partial_rows`,
		gymID, rootID, gymID, unrelatedID,
		rootRewritePayment, unrelatedPayment, rootRewriteRefund, unrelatedRefund,
		rootRewritePayment, unrelatedPayment, rootRewriteRefund, unrelatedRefund).Scan(&canonical).Error; err != nil {
		t.Fatal(err)
	}
	if canonical.RootCents != 10000 || canonical.UnrelatedCents != 20000 || canonical.PartialRows != 0 {
		t.Fatalf("financial graph guard left canonical changes: %+v", canonical)
	}
}

func TestSaleCorrectionAndOvercollectionRefund_CommitAsOneGraph(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	rootID, saleID, correctionID, refundPaymentID, refundID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 1, ?, ?, 'PRD-SYNC-CORRECTION-GRAPH', 100, 100, 'transfer', 'product', 0, 0, ?, ?)`,
		rootID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sales(
		id,gym_id,version,created_at,updated_at,payment_id,subtotal,discount,total,correction_version)
		VALUES (?, ?, 1, ?, ?, ?, 100, 0, 100, 0)`, saleID, gymID, now, now, rootID).Error; err != nil {
		t.Fatal(err)
	}
	graphIDs := []string{refundID.String()}
	encode := func(entityType string, id uuid.UUID, payload map[string]any) PushItem {
		payload[refundGraphIDsKey] = graphIDs
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		version, _ := numericInt(payload["version"])
		return PushItem{QueueID: uuid.NewString(), EntityType: entityType, EntityID: id.String(),
			Operation: OpUpsertStr, ClientVersion: version, Payload: encoded, EnqueuedAt: now}
	}
	payment := map[string]any{
		"id": rootID.String(), "gym_id": gymID.String(), "version": 2,
		"created_at": now.UnixMilli(), "updated_at": now.Add(time.Second).UnixMilli(), "deleted_at": nil,
		"folio": "PRD-SYNC-CORRECTION-GRAPH", "member_id": nil, "membership_id": nil,
		"amount": 100.0, "recognized_amount": 60.0, "payment_method": "transfer", "concept": "product",
		"parent_payment_id": nil, "discount_amount": 40.0, "balance_pending": 0,
		"payment_date": now.Format("2006-01-02"), "operator_id": userID.String(),
	}
	sale := map[string]any{
		"id": saleID.String(), "gym_id": gymID.String(), "version": 2,
		"created_at": now.UnixMilli(), "updated_at": now.Add(time.Second).UnixMilli(), "deleted_at": nil,
		"payment_id": rootID.String(), "member_id": nil,
		"subtotal": 100.0, "discount": 40.0, "total": 60.0, "correction_version": 1,
	}
	before := map[string]any{
		"sale_id": saleID.String(), "correction_version": 0,
		"subtotal_cents": 10000, "discount_cents": 0, "total_cents": 10000,
		"payment_amount_cents": 10000, "recognized_amount_cents": 10000, "payment_tombstoned": false,
	}
	after := map[string]any{
		"sale_id": saleID.String(), "correction_version": 1,
		"subtotal_cents": 10000, "discount_cents": 4000, "total_cents": 6000,
		"payment_amount_cents": 10000, "recognized_amount_cents": 6000, "payment_tombstoned": false,
	}
	correction := map[string]any{
		"id": correctionID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.Add(time.Second).UnixMilli(), "updated_at": now.Add(time.Second).UnixMilli(), "deleted_at": nil,
		"sale_id": saleID.String(), "expected_sale_version": 0,
		"reason": "El total real era menor", "correction_type": "edit", "money_resolution": "refund_excess",
		"increase_resolution": nil, "monetary_delta": 40.0, "before_snapshot": before, "after_snapshot": after,
		"idempotency_key": "correction:refund-graph", "idempotency_fingerprint": "fingerprint:correction-refund-graph",
		"idempotency_result": map[string]any{"correction_id": correctionID.String()}, "created_by": userID.String(),
	}
	negative := map[string]any{
		"id": refundPaymentID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.Add(time.Second).UnixMilli(), "updated_at": now.Add(time.Second).UnixMilli(), "deleted_at": nil,
		"folio": "RFD-SYNC-CORRECTION-GRAPH", "member_id": nil, "membership_id": nil,
		"amount": -40.0, "recognized_amount": 0, "payment_method": "transfer", "concept": "refund",
		"parent_payment_id": rootID.String(), "discount_amount": 0, "balance_pending": 0,
		"payment_date": now.Format("2006-01-02"), "operator_id": userID.String(),
	}
	refund := map[string]any{
		"id": refundID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.Add(time.Second).UnixMilli(), "updated_at": now.Add(time.Second).UnixMilli(), "deleted_at": nil,
		"root_payment_id": rootID.String(), "refund_payment_id": refundPaymentID.String(), "sale_id": saleID.String(),
		"amount": 40.0, "method": "transfer", "refunded_on": now.Format("2006-01-02"),
		"reason": "El total real era menor", "kind": "overcollection_settlement", "balance_cancelled": 0,
		"legacy_incomplete": false, "correction_id": correctionID.String(),
		"idempotency_key": "correction:refund-graph:settlement", "idempotency_fingerprint": "fingerprint:refund-graph:settlement",
		"idempotency_result": map[string]any{"refund_id": refundID.String(), "amount": 40}, "created_by": userID.String(),
		refundGraphExpectedItemsKey: 0,
	}
	request := PushRequest{ClientID: uuid.NewString(), ClientNow: now.Add(time.Second), SchemaVersion: SchemaVersion,
		Batch: []PushItem{
			encode("payments", rootID, payment), encode("sales", saleID, sale),
			encode("sale_corrections", correctionID, correction), encode("payments", refundPaymentID, negative),
			encode("refunds", refundID, refund),
		}}
	response, status, err := performSyncPush(router, token, request)
	if err != nil || status != http.StatusOK || len(response.Results) != len(request.Batch) {
		t.Fatalf("correction refund graph status=%d err=%v response=%+v", status, err, response)
	}
	for _, result := range response.Results {
		if result.Status != StatusAccepted {
			t.Fatalf("correction refund graph row=%+v", result)
		}
	}
	var canonical struct {
		Recognized  int64 `gorm:"column:recognized"`
		Refunds     int64 `gorm:"column:refunds"`
		Corrections int64 `gorm:"column:corrections"`
	}
	if err := db.Raw(`SELECT ROUND(recognized_amount*100)::bigint AS recognized,
		(SELECT COUNT(*) FROM refunds WHERE id=? AND deleted_at IS NULL) AS refunds,
		(SELECT COUNT(*) FROM sale_corrections WHERE id=? AND deleted_at IS NULL) AS corrections
		FROM payments WHERE gym_id=? AND id=?`, refundID, correctionID, gymID, rootID).Scan(&canonical).Error; err != nil {
		t.Fatal(err)
	}
	if canonical.Recognized != 6000 || canonical.Refunds != 1 || canonical.Corrections != 1 {
		t.Fatalf("canonical correction/refund graph=%+v", canonical)
	}
}

func clonePayloadMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func TestTwoSidecars_ConcurrentDebtCancellationsShareOnePendingBalance(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	paymentID, saleID, productID, saleItemID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	encodeItem := func(entityType string, id uuid.UUID, version int, payload map[string]any) PushItem {
		t.Helper()
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: entityType, EntityID: id.String(),
			Operation: OpUpsertStr, ClientVersion: version, Payload: encoded, EnqueuedAt: now}
	}
	payment := map[string]any{
		"id": paymentID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
		"folio": "PRD-SYNC-PENDING", "amount": 80.0, "recognized_amount": 80.0,
		"payment_method": "transfer", "concept": "product", "discount_amount": 0,
		"balance_pending": 20.0, "payment_date": now.Format("2006-01-02"), "operator_id": userID.String(),
	}
	sale := map[string]any{
		"id": saleID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
		"payment_id": paymentID.String(), "member_id": nil,
		"subtotal": 100.0, "discount": 0, "total": 100.0, "correction_version": 0,
	}
	seed := PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion,
		Batch: []PushItem{encodeItem("payments", paymentID, 1, payment), encodeItem("sales", saleID, 1, sale)}}
	seedResponse, status, err := performSyncPush(router, token, seed)
	if err != nil || status != http.StatusOK || len(seedResponse.Results) != 2 ||
		seedResponse.Results[0].Status != StatusAccepted || seedResponse.Results[1].Status != StatusAccepted {
		t.Fatalf("seed graph status=%d err=%v response=%+v", status, err, seedResponse)
	}
	if err := db.Exec(`INSERT INTO products(
		id,gym_id,version,created_at,updated_at,name,price,stock,stock_minimum,active)
		VALUES (?, ?, 1, ?, ?, 'Producto deuda sync', 20, 5, 0, TRUE)`,
		productID, gymID, now, now).Error; err != nil {
		t.Fatalf("seed debt product: %v", err)
	}
	if err := db.Exec(`INSERT INTO sale_items(
		id,gym_id,version,created_at,updated_at,sale_id,product_id,product_name_snapshot,
		unit_price_snapshot,unit_cost_snapshot,quantity,line_total)
		VALUES (?, ?, 1, ?, ?, ?, ?, 'Producto deuda sync', 20, 10, 5, 100)`,
		saleItemID, gymID, now, now, saleID, productID).Error; err != nil {
		t.Fatalf("seed debt sale item: %v", err)
	}

	makeRequest := func(label string) PushRequest {
		refundID, refundItemID := uuid.New(), uuid.New()
		key := "refund:cancel-pending:" + label
		refund := map[string]any{
			"id": refundID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"root_payment_id": paymentID.String(), "refund_payment_id": nil, "sale_id": saleID.String(),
			"amount": 0, "method": nil, "refunded_on": now.Format("2006-01-02"),
			"reason": "Cancela saldo simultáneo " + label, "kind": "revenue_refund", "balance_cancelled": 20.0,
			"legacy_incomplete": false, "correction_id": nil,
			"idempotency_key": key, "idempotency_fingerprint": "fingerprint:" + label,
			"idempotency_result": map[string]any{"refund_id": refundID.String(), "balance_cancelled": 20},
			"created_by":         userID.String(),
		}
		refundItem := map[string]any{
			"id": refundItemID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"refund_id": refundID.String(), "sale_item_id": saleItemID.String(),
			"quantity": 1, "amount": 20.0, "disposition": "not_returned",
		}
		return PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion,
			Batch: []PushItem{
				encodeItem("refunds", refundID, 1, refund),
				encodeItem("refund_items", refundItemID, 1, refundItem),
			}}
	}
	requests := []PushRequest{makeRequest("A"), makeRequest("B")}
	type outcome struct {
		response PushResponse
		status   int
		err      error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(requests))
	for _, request := range requests {
		request := request
		go func() {
			<-start
			response, status, pushErr := performSyncPush(router, token, request)
			outcomes <- outcome{response: response, status: status, err: pushErr}
		}()
	}
	close(start)
	accepted, rejected := 0, 0
	for range requests {
		out := <-outcomes
		if out.err != nil || out.status != http.StatusOK || len(out.response.Results) != 2 {
			t.Fatalf("debt cancellation status=%d err=%v response=%+v", out.status, out.err, out.response)
		}
		if out.response.Results[0].Status != out.response.Results[1].Status {
			t.Fatalf("debt cancellation graph was partial: %+v", out.response.Results)
		}
		switch out.response.Results[0].Status {
		case StatusAccepted:
			accepted++
		case StatusRejectedFinancialConflict:
			rejected++
		default:
			t.Fatalf("unexpected debt cancellation result: %+v", out.response.Results[0])
		}
	}
	var canonical struct {
		PendingCents int64 `gorm:"column:pending_cents"`
		PaymentVer   int   `gorm:"column:payment_version"`
		JournalVer   int   `gorm:"column:journal_version"`
		JournalCents int64 `gorm:"column:journal_cents"`
	}
	if err := db.Raw(`SELECT ROUND(p.balance_pending*100)::bigint AS pending_cents,
		p.version AS payment_version,se.version AS journal_version,
		ROUND((se.payload->>'balance_pending')::numeric*100)::bigint AS journal_cents
		FROM payments p JOIN sync_entities se ON se.gym_id=p.gym_id
		  AND se.entity_type='payments' AND se.entity_id=p.id
		WHERE p.gym_id=? AND p.id=?`, gymID, paymentID).Scan(&canonical).Error; err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || rejected != 1 || canonical.PendingCents != 0 || canonical.JournalCents != 0 ||
		canonical.JournalVer != canonical.PaymentVer || canonical.PaymentVer <= 1 {
		t.Fatalf("accepted=%d rejected=%d canonical=%+v", accepted, rejected, canonical)
	}
}

func TestTwoSidecars_ConcurrentSaleCorrectionsClaimOneRevision(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	paymentID, saleID := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 2, ?, ?, 'PRD-SYNC-CORR-RACE', 40, 40, 'transfer', 'product', 10, 0, ?, ?)`,
		paymentID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatalf("seed correction payment: %v", err)
	}
	if err := db.Exec(`INSERT INTO sales(
		id,gym_id,version,created_at,updated_at,payment_id,subtotal,discount,total,correction_version)
		VALUES (?, ?, 2, ?, ?, ?, 50, 10, 40, 1)`,
		saleID, gymID, now, now, paymentID).Error; err != nil {
		t.Fatalf("seed corrected sale: %v", err)
	}

	makeRequest := func(label string) PushRequest {
		id := uuid.New()
		key := "correction:concurrent:" + label
		payload, marshalErr := json.Marshal(map[string]any{
			"id": id.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"sale_id": saleID.String(), "expected_sale_version": 0,
			"reason": "Corrección simultánea " + label, "money_resolution": "record_only", "monetary_delta": 10,
			"before_snapshot": map[string]any{"sale_id": saleID.String(), "correction_version": 0,
				"subtotal_cents": 5000, "discount_cents": 0, "total_cents": 5000},
			"after_snapshot": map[string]any{"sale_id": saleID.String(), "correction_version": 1,
				"subtotal_cents": 5000, "discount_cents": 1000, "total_cents": 4000},
			"idempotency_key": key, "idempotency_fingerprint": "fingerprint:" + label,
			"idempotency_result": map[string]any{"correction_id": id.String()}, "created_by": userID.String(),
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion,
			Batch: []PushItem{{QueueID: uuid.NewString(), EntityType: "sale_corrections", EntityID: id.String(),
				Operation: OpUpsertStr, ClientVersion: 1, Payload: payload, EnqueuedAt: now}}}
	}
	requests := []PushRequest{makeRequest("A"), makeRequest("B")}
	type outcome struct {
		response PushResponse
		status   int
		err      error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(requests))
	for _, request := range requests {
		request := request
		go func() {
			<-start
			response, status, pushErr := performSyncPush(router, token, request)
			outcomes <- outcome{response: response, status: status, err: pushErr}
		}()
	}
	close(start)
	accepted, rejected := 0, 0
	for range requests {
		out := <-outcomes
		if out.err != nil || out.status != http.StatusOK || len(out.response.Results) != 1 {
			t.Fatalf("concurrent correction status=%d err=%v response=%+v", out.status, out.err, out.response)
		}
		switch out.response.Results[0].Status {
		case StatusAccepted:
			accepted++
		case StatusRejectedFinancialConflict:
			rejected++
		default:
			t.Fatalf("unexpected correction result: %+v", out.response.Results[0])
		}
	}
	var canonical int64
	if err := db.Table("sale_corrections").
		Where("gym_id=? AND sale_id=? AND expected_sale_version=0 AND deleted_at IS NULL", gymID, saleID).
		Count(&canonical).Error; err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || rejected != 1 || canonical != 1 {
		t.Fatalf("accepted=%d rejected=%d canonical=%d, want 1/1/1", accepted, rejected, canonical)
	}
}

func TestTwoSidecars_ConcurrentAdministrativePaymentCorrectionsClaimOneVersion(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	paymentID := uuid.New()
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 1, ?, ?, 'OTH-SYNC-CORR-RACE', 100, 100, 'transfer', 'other', 0, 0, ?, ?)`,
		paymentID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatal(err)
	}
	before := map[string]any{
		"version": 1, "amount": 100.0, "recognized_amount": 100.0, "balance_pending": 0,
		"payment_method": "transfer", "cash_drawer_id": nil, "payment_date": now.Format("2006-01-02"), "annulled": false,
	}
	type candidate struct {
		label, method string
		amount        float64
		updatedAt     time.Time
		correctionID  uuid.UUID
	}
	candidates := []candidate{
		{label: "A", method: "card", amount: 90, updatedAt: now.Add(time.Second), correctionID: uuid.New()},
		{label: "B", method: "transfer", amount: 80, updatedAt: now.Add(2 * time.Second), correctionID: uuid.New()},
	}
	makeRequest := func(c candidate) PushRequest {
		after := map[string]any{
			"version": 2, "amount": c.amount, "recognized_amount": c.amount, "balance_pending": 0,
			"payment_method": c.method, "cash_drawer_id": nil, "payment_date": now.Format("2006-01-02"), "annulled": false,
		}
		paymentPayload, marshalErr := json.Marshal(map[string]any{
			"id": paymentID.String(), "gym_id": gymID.String(), "version": 2,
			"created_at": now.UnixMilli(), "updated_at": c.updatedAt.UnixMilli(), "deleted_at": nil,
			"folio": "OTH-SYNC-CORR-RACE", "member_id": nil, "membership_id": nil,
			"amount": c.amount, "recognized_amount": c.amount, "payment_method": c.method, "concept": "other",
			"parent_payment_id": nil, "discount_amount": 0, "balance_pending": 0,
			"payment_date": now.Format("2006-01-02"), "operator_id": userID.String(), "cash_drawer_id": nil,
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		correctionPayload, marshalErr := json.Marshal(map[string]any{
			"id": c.correctionID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": c.updatedAt.UnixMilli(), "updated_at": c.updatedAt.UnixMilli(), "deleted_at": nil,
			"payment_id": paymentID.String(), "expected_payment_version": 1,
			"reason":          "Corrección administrativa " + c.label,
			"before_snapshot": before, "after_snapshot": after,
			"idempotency_key":         "payment-correction:concurrent:" + c.label,
			"idempotency_fingerprint": "payment-correction-fingerprint:" + c.label,
			"idempotency_result":      map[string]any{"correction_id": c.correctionID.String()}, "created_by": userID.String(),
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushRequest{ClientID: uuid.NewString(), ClientNow: c.updatedAt, SchemaVersion: SchemaVersion,
			Batch: []PushItem{
				{QueueID: uuid.NewString(), EntityType: "payments", EntityID: paymentID.String(), Operation: OpUpsertStr,
					ClientVersion: 2, Payload: paymentPayload, EnqueuedAt: c.updatedAt},
				{QueueID: uuid.NewString(), EntityType: "payment_corrections", EntityID: c.correctionID.String(), Operation: OpUpsertStr,
					ClientVersion: 1, Payload: correctionPayload, EnqueuedAt: c.updatedAt},
			}}
	}
	type outcome struct {
		response PushResponse
		status   int
		err      error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(candidates))
	for _, candidate := range candidates {
		request := makeRequest(candidate)
		go func() {
			<-start
			response, status, pushErr := performSyncPush(router, token, request)
			outcomes <- outcome{response: response, status: status, err: pushErr}
		}()
	}
	close(start)
	accepted, rejected := 0, 0
	for range candidates {
		out := <-outcomes
		if out.err != nil || out.status != http.StatusOK || len(out.response.Results) != 2 {
			t.Fatalf("concurrent payment correction status=%d err=%v response=%+v", out.status, out.err, out.response)
		}
		switch out.response.Results[1].Status {
		case StatusAccepted:
			accepted++
		case StatusRejectedFinancialConflict:
			rejected++
		default:
			t.Fatalf("unexpected administrative correction result: %+v", out.response.Results[1])
		}
	}
	var canonical struct {
		AmountCents        int64           `gorm:"column:amount_cents"`
		Method             string          `gorm:"column:payment_method"`
		After              json.RawMessage `gorm:"column:after_snapshot"`
		CorrectionCount    int64           `gorm:"column:correction_count"`
		JournalAmountCents int64           `gorm:"column:journal_amount_cents"`
	}
	if err := db.Raw(`SELECT ROUND(p.amount*100)::bigint AS amount_cents,p.payment_method,
		c.after_snapshot,(SELECT COUNT(*) FROM payment_corrections all_c
		  WHERE all_c.gym_id=p.gym_id AND all_c.payment_id=p.id AND all_c.deleted_at IS NULL) AS correction_count,
		ROUND((se.payload->>'amount')::numeric*100)::bigint AS journal_amount_cents
		FROM payments p JOIN payment_corrections c ON c.gym_id=p.gym_id AND c.payment_id=p.id AND c.deleted_at IS NULL
		JOIN sync_entities se ON se.gym_id=p.gym_id AND se.entity_type='payments' AND se.entity_id=p.id
		WHERE p.gym_id=? AND p.id=?`, gymID, paymentID).Scan(&canonical).Error; err != nil {
		t.Fatal(err)
	}
	after, err := projectorSnapshot(canonical.After)
	if err != nil {
		t.Fatal(err)
	}
	winner, err := projectorAdministrativePaymentSnapshot(after)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || rejected != 1 || canonical.CorrectionCount != 1 ||
		canonical.AmountCents != winner.AmountCents || canonical.Method != winner.Method ||
		canonical.JournalAmountCents != canonical.AmountCents {
		t.Fatalf("accepted=%d rejected=%d canonical=%+v winner=%+v", accepted, rejected, canonical, winner)
	}
}

func TestOfflineOtherIncomeAnnul_ConvergesAndCannotBeResurrected(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	annulledAt := now.Add(time.Second)
	paymentID, correctionID := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 1, ?, ?, 'OTH-SYNC-ANNUL', 75, 75, 'transfer', 'other', 0, 0, ?, ?)`,
		paymentID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatal(err)
	}
	paymentPayload := map[string]any{
		"id": paymentID.String(), "gym_id": gymID.String(), "version": 2,
		"created_at": now.UnixMilli(), "updated_at": annulledAt.UnixMilli(), "deleted_at": annulledAt.UnixMilli(),
		"folio": "OTH-SYNC-ANNUL", "member_id": nil, "membership_id": nil,
		"amount": 75.0, "recognized_amount": 75.0, "payment_method": "transfer", "concept": "other",
		"parent_payment_id": nil, "discount_amount": 0, "balance_pending": 0,
		"payment_date": now.Format("2006-01-02"), "operator_id": userID.String(), "cash_drawer_id": nil,
	}
	before := map[string]any{
		"version": 1, "amount": 75.0, "recognized_amount": 75.0, "balance_pending": 0,
		"payment_method": "transfer", "cash_drawer_id": nil, "payment_date": now.Format("2006-01-02"), "annulled": false,
	}
	after := map[string]any{
		"version": 2, "amount": 75.0, "recognized_amount": 75.0, "balance_pending": 0,
		"payment_method": "transfer", "cash_drawer_id": nil, "payment_date": now.Format("2006-01-02"), "annulled": true,
	}
	correctionPayload := map[string]any{
		"id": correctionID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": annulledAt.UnixMilli(), "updated_at": annulledAt.UnixMilli(), "deleted_at": nil,
		"payment_id": paymentID.String(), "expected_payment_version": 1,
		"reason": "El ingreso extraordinario nunca ocurrió", "before_snapshot": before, "after_snapshot": after,
		"idempotency_key": "payment-correction:annul", "idempotency_fingerprint": "payment-correction-fingerprint:annul",
		"idempotency_result": map[string]any{"correction_id": correctionID.String(), "annulled": true},
		"created_by":         userID.String(),
	}
	makeItem := func(entityType string, entityID uuid.UUID, version int, value map[string]any) PushItem {
		t.Helper()
		payload, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: entityType, EntityID: entityID.String(),
			Operation: OpUpsertStr, ClientVersion: version, Payload: payload, EnqueuedAt: annulledAt}
	}
	request := PushRequest{ClientID: uuid.NewString(), ClientNow: annulledAt, SchemaVersion: SchemaVersion,
		Batch: []PushItem{
			makeItem("payments", paymentID, 2, paymentPayload),
			makeItem("payment_corrections", correctionID, 1, correctionPayload),
		}}
	response, status, err := performSyncPush(router, token, request)
	if err != nil || status != http.StatusOK || len(response.Results) != 2 {
		t.Fatalf("annul other income status=%d err=%v response=%+v", status, err, response)
	}
	for _, result := range response.Results {
		if result.Status != StatusAccepted {
			t.Fatalf("annul other income row=%+v, want accepted", result)
		}
	}
	var canonical struct {
		Deleted     bool  `gorm:"column:deleted"`
		AmountCents int64 `gorm:"column:amount_cents"`
		Corrections int64 `gorm:"column:corrections"`
	}
	if err := db.Raw(`SELECT (p.deleted_at IS NOT NULL) AS deleted,
		ROUND(p.amount*100)::bigint AS amount_cents,
		(SELECT COUNT(*) FROM payment_corrections c WHERE c.gym_id=p.gym_id AND c.payment_id=p.id AND c.deleted_at IS NULL) AS corrections
		FROM payments p WHERE p.gym_id=? AND p.id=?`, gymID, paymentID).Scan(&canonical).Error; err != nil {
		t.Fatal(err)
	}
	if !canonical.Deleted || canonical.AmountCents != 7500 || canonical.Corrections != 1 {
		t.Fatalf("canonical other-income annul=%+v", canonical)
	}

	// A desk that missed the correction may present a larger generic row
	// version. Annulment is terminal: version inflation cannot resurrect it.
	resurrectedAt := annulledAt.Add(time.Second)
	paymentPayload["version"], paymentPayload["updated_at"], paymentPayload["deleted_at"] = 3, resurrectedAt.UnixMilli(), nil
	late := PushRequest{ClientID: uuid.NewString(), ClientNow: resurrectedAt, SchemaVersion: SchemaVersion,
		Batch: []PushItem{makeItem("payments", paymentID, 3, paymentPayload)}}
	lateResponse, status, err := performSyncPush(router, token, late)
	if err != nil || status != http.StatusOK || len(lateResponse.Results) != 1 ||
		lateResponse.Results[0].Status != StatusRejectedFinancialConflict {
		t.Fatalf("resurrect other income status=%d err=%v response=%+v", status, err, lateResponse)
	}
}

func TestPaymentCorrections_CrossGymPushRejectedAndFullSyncScoped(t *testing.T) {
	db := projectorTestDB(t)
	gymA, userA := seedGymAndOwner(t, db)
	gymB, userB := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	tokenA, err := tokens.GenerateAccessToken(userA, gymA, "owner")
	if err != nil {
		t.Fatal(err)
	}
	tokenB, err := tokens.GenerateAccessToken(userB, gymB, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	paymentA, paymentB := uuid.New(), uuid.New()
	for _, seed := range []struct {
		gymID, userID, paymentID uuid.UUID
		folio                    string
	}{{gymA, userA, paymentA, "OTH-SCOPE-A"}, {gymB, userB, paymentB, "OTH-SCOPE-B"}} {
		if err := db.Exec(`INSERT INTO payments(
			id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
			payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
			VALUES (?, ?, 2, ?, ?, ?, 90, 90, 'card', 'other', 0, 0, ?, ?)`,
			seed.paymentID, seed.gymID, now, now, seed.folio, now, seed.userID).Error; err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]any{
		"version": 1, "amount": 100.0, "recognized_amount": 100.0, "balance_pending": 0,
		"payment_method": "transfer", "cash_drawer_id": nil, "payment_date": now.Format("2006-01-02"), "annulled": false,
	}
	after := map[string]any{
		"version": 2, "amount": 90.0, "recognized_amount": 90.0, "balance_pending": 0,
		"payment_method": "card", "cash_drawer_id": nil, "payment_date": now.Format("2006-01-02"), "annulled": false,
	}
	makeCorrection := func(gymID, userID, paymentID, correctionID uuid.UUID, label string) PushItem {
		t.Helper()
		payload, marshalErr := json.Marshal(map[string]any{
			"id": correctionID.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
			"payment_id": paymentID.String(), "expected_payment_version": 1,
			"reason": "Corrección de alcance " + label, "before_snapshot": before, "after_snapshot": after,
			"idempotency_key":         "payment-correction:scope:" + label,
			"idempotency_fingerprint": "payment-correction-scope-fingerprint:" + label,
			"idempotency_result":      map[string]any{"correction_id": correctionID.String()}, "created_by": userID.String(),
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: "payment_corrections", EntityID: correctionID.String(),
			Operation: OpUpsertStr, ClientVersion: 1, Payload: payload, EnqueuedAt: now}
	}
	push := func(token string, item PushItem) PushItemResult {
		t.Helper()
		response, status, pushErr := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), ClientNow: now,
			SchemaVersion: SchemaVersion, Batch: []PushItem{item}})
		if pushErr != nil || status != http.StatusOK || len(response.Results) != 1 {
			t.Fatalf("push scoped correction status=%d err=%v response=%+v", status, pushErr, response)
		}
		return response.Results[0]
	}
	correctionA, correctionB := uuid.New(), uuid.New()
	// created_by is also tenant-owned evidence. A token from gym A cannot
	// attribute an otherwise valid correction to an operator from gym B.
	crossAuthorID := uuid.New()
	if result := push(tokenA, makeCorrection(gymA, userB, paymentA, crossAuthorID, "cross-author")); result.Status != StatusRejectedFinancialConflict || !strings.Contains(result.Error, "autor") {
		t.Fatalf("cross-gym correction author=%+v, want author financial conflict", result)
	}
	if result := push(tokenA, makeCorrection(gymA, userA, paymentA, correctionA, "A")); result.Status != StatusAccepted {
		t.Fatalf("correction A=%+v", result)
	}
	if result := push(tokenB, makeCorrection(gymB, userB, paymentB, correctionB, "B")); result.Status != StatusAccepted {
		t.Fatalf("correction B=%+v", result)
	}

	// The payload's gym matches the authenticated gym, but its FK points at
	// another tenant. The aggregate projector must reject before either the
	// domain row or sync journal can materialise it.
	crossID := uuid.New()
	cross := makeCorrection(gymA, userA, paymentB, crossID, "cross")
	if result := push(tokenA, cross); result.Status != StatusRejectedFinancialConflict {
		t.Fatalf("cross-gym correction=%+v, want financial conflict", result)
	}
	var crossRows int64
	if err := db.Raw(`SELECT
		(SELECT COUNT(*) FROM payment_corrections WHERE id=?) +
		(SELECT COUNT(*) FROM sync_entities WHERE entity_type='payment_corrections' AND entity_id=?)`,
		crossID, crossID).Scan(&crossRows).Error; err != nil {
		t.Fatal(err)
	}
	if crossRows != 0 {
		t.Fatalf("cross-gym correction left %d rows", crossRows)
	}

	assertFullSync := func(token string, want, forbidden uuid.UUID) {
		t.Helper()
		recorder := doJSONReq(t, router, http.MethodGet, "/api/v1/sync/full?limit=5000", token, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("full sync status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response FullSyncResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, change := range response.Changes {
			if change.EntityType != "payment_corrections" {
				continue
			}
			if change.EntityID == forbidden.String() || change.EntityID == crossID.String() {
				t.Fatalf("full sync leaked correction %s", change.EntityID)
			}
			if change.EntityID == want.String() {
				found = true
			}
		}
		if !found {
			t.Fatalf("full sync did not include own correction %s", want)
		}
	}
	assertFullSync(tokenA, correctionA, correctionB)
	assertFullSync(tokenB, correctionB, correctionA)
}

// A single sidecar may make several corrections while offline. Its mutable
// Sale queue row is coalesced to revision 2, but the two immutable audit rows
// remain 0->1 and 1->2. The projector must accept that exact chain in order
// instead of mistaking revision 0 for a competing stale edit.
func TestOfflineCorrectionChain_ConvergesAgainstCoalescedSale(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	paymentID, saleID := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 3, ?, ?, 'PRD-SYNC-CORR-CHAIN', 30, 30, 'transfer', 'product', 20, 0, ?, ?)`,
		paymentID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sales(
		id,gym_id,version,created_at,updated_at,payment_id,subtotal,discount,total,correction_version)
		VALUES (?, ?, 3, ?, ?, ?, 50, 20, 30, 2)`,
		saleID, gymID, now, now, paymentID).Error; err != nil {
		t.Fatal(err)
	}
	snapshot := func(version int, discount, total int64) map[string]any {
		return map[string]any{"sale_id": saleID.String(), "correction_version": version,
			"subtotal_cents": int64(5000), "discount_cents": discount, "total_cents": total}
	}
	makeItem := func(expected int, before, after map[string]any) PushItem {
		id := uuid.New()
		key := "correction:chain:" + strconv.Itoa(expected)
		payload, marshalErr := json.Marshal(map[string]any{
			"id": id.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": now.Add(time.Duration(expected) * time.Second).UnixMilli(),
			"updated_at": now.Add(time.Duration(expected) * time.Second).UnixMilli(), "deleted_at": nil,
			"sale_id": saleID.String(), "expected_sale_version": expected,
			"reason": "Corrección offline encadenada", "money_resolution": "record_only", "monetary_delta": 10,
			"before_snapshot": before, "after_snapshot": after,
			"idempotency_key": key, "idempotency_fingerprint": "fingerprint:" + key,
			"idempotency_result": map[string]any{"correction_id": id.String()}, "created_by": userID.String(),
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: "sale_corrections", EntityID: id.String(),
			Operation: OpUpsertStr, ClientVersion: 1, Payload: payload, EnqueuedAt: now}
	}
	request := PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion,
		Batch: []PushItem{
			makeItem(0, snapshot(0, 0, 5000), snapshot(1, 1000, 4000)),
			makeItem(1, snapshot(1, 1000, 4000), snapshot(2, 2000, 3000)),
		}}
	response, status, err := performSyncPush(router, token, request)
	if err != nil || status != http.StatusOK || len(response.Results) != 2 {
		t.Fatalf("push chain status=%d err=%v response=%+v", status, err, response)
	}
	for i, result := range response.Results {
		if result.Status != StatusAccepted {
			t.Fatalf("chain transition %d=%+v, want accepted", i, result)
		}
	}
}

// An annulment is still a correction aggregate even though its mutable Sale,
// SaleItem and (for record_only) Payment arrive as tombstones before the
// immutable audit row. The projector must lock and validate those tombstones,
// then prevent an older desk from resurrecting any part of the graph.
func TestOfflineRecordOnlyAnnul_ConvergesAndCannotBeResurrected(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	annulledAt := now.Add(10 * time.Second)
	paymentID, saleID, saleItemID, productID, correctionID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO products(
		id,gym_id,version,created_at,updated_at,name,price,stock,stock_minimum,active)
		VALUES (?, ?, 1, ?, ?, 'Producto anulado', 20, 9, 1, TRUE)`,
		productID, gymID, now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 1, ?, ?, 'PRD-SYNC-ANNUL', 20, 20, 'cash', 'product', 0, 0, ?, ?)`,
		paymentID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sales(
		id,gym_id,version,created_at,updated_at,payment_id,subtotal,discount,total,correction_version)
		VALUES (?, ?, 1, ?, ?, ?, 20, 0, 20, 0)`,
		saleID, gymID, now, now, paymentID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sale_items(
		id,gym_id,version,created_at,updated_at,sale_id,product_id,product_name_snapshot,
		unit_price_snapshot,unit_cost_snapshot,quantity,line_total)
		VALUES (?, ?, 1, ?, ?, ?, ?, 'Producto anulado', 20, 5, 1, 20)`,
		saleItemID, gymID, now, now, saleID, productID).Error; err != nil {
		t.Fatal(err)
	}

	lineSnapshot := map[string]any{
		"sale_item_id": saleItemID.String(), "product_id": productID.String(),
		"product_name": "Producto anulado", "unit_price_cents": 2000, "unit_cost_cents": 500,
		"quantity": 1, "line_total_cents": 2000,
	}
	salePayload := map[string]any{
		"id": saleID.String(), "gym_id": gymID.String(), "version": 2,
		"created_at": now.UnixMilli(), "updated_at": annulledAt.UnixMilli(), "deleted_at": annulledAt.UnixMilli(),
		"payment_id": paymentID.String(), "member_id": nil,
		"subtotal": 0, "discount": 0, "total": 0, "correction_version": 1,
	}
	saleItemPayload := map[string]any{
		"id": saleItemID.String(), "gym_id": gymID.String(), "version": 2,
		"created_at": now.UnixMilli(), "updated_at": annulledAt.UnixMilli(), "deleted_at": annulledAt.UnixMilli(),
		"sale_id": saleID.String(), "product_id": productID.String(), "product_name_snapshot": "Producto anulado",
		"unit_price_snapshot": 20.0, "unit_cost_snapshot": 5.0, "quantity": 1, "line_total": 20.0,
	}
	paymentPayload := map[string]any{
		"id": paymentID.String(), "gym_id": gymID.String(), "version": 2,
		"created_at": now.UnixMilli(), "updated_at": annulledAt.UnixMilli(), "deleted_at": annulledAt.UnixMilli(),
		"folio": "PRD-SYNC-ANNUL", "amount": 20.0, "recognized_amount": 20.0,
		"payment_method": "cash", "concept": "product", "discount_amount": 0,
		"balance_pending": 0, "payment_date": now.Format("2006-01-02"), "operator_id": userID.String(),
	}
	before := map[string]any{
		"sale_id": saleID.String(), "payment_id": paymentID.String(), "correction_version": 0,
		"annulled": false, "payment_tombstoned": false,
		"subtotal_cents": 2000, "discount_cents": 0, "total_cents": 2000,
		"payment_amount_cents": 2000, "recognized_amount_cents": 2000,
		"lines": []any{lineSnapshot},
	}
	after := map[string]any{
		"sale_id": saleID.String(), "payment_id": paymentID.String(), "correction_version": 1,
		"annulled": true, "payment_tombstoned": true,
		"subtotal_cents": 0, "discount_cents": 0, "total_cents": 0,
		"payment_amount_cents": 2000, "recognized_amount_cents": 2000,
		"lines": []any{},
	}
	correctionPayload := map[string]any{
		"id": correctionID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": annulledAt.UnixMilli(), "updated_at": annulledAt.UnixMilli(), "deleted_at": nil,
		"sale_id": saleID.String(), "expected_sale_version": 0, "reason": "La venta nunca ocurrió",
		"correction_type": "annul", "money_resolution": "record_only", "increase_resolution": nil,
		"monetary_delta": 20.0, "before_snapshot": before, "after_snapshot": after,
		"idempotency_key": "correction:annul-record-only", "idempotency_fingerprint": "fingerprint:annul-record-only",
		"idempotency_result": map[string]any{"correction_id": correctionID.String(), "annulled": true},
		"created_by":         userID.String(),
	}
	makePushItem := func(entityType string, id uuid.UUID, version int, payload map[string]any) PushItem {
		t.Helper()
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: entityType, EntityID: id.String(),
			Operation: OpUpsertStr, ClientVersion: version, Payload: encoded, EnqueuedAt: annulledAt}
	}
	request := PushRequest{ClientID: uuid.NewString(), ClientNow: annulledAt, SchemaVersion: SchemaVersion,
		Batch: []PushItem{
			makePushItem("sales", saleID, 2, salePayload),
			makePushItem("sale_items", saleItemID, 2, saleItemPayload),
			makePushItem("payments", paymentID, 2, paymentPayload),
			makePushItem("sale_corrections", correctionID, 1, correctionPayload),
		}}
	response, status, err := performSyncPush(router, token, request)
	if err != nil || status != http.StatusOK || len(response.Results) != len(request.Batch) {
		t.Fatalf("annul graph status=%d err=%v response=%+v", status, err, response)
	}
	for _, result := range response.Results {
		if result.Status != StatusAccepted {
			t.Fatalf("annul graph row=%+v, want accepted", result)
		}
	}

	var canonical struct {
		SaleDeleted    bool  `gorm:"column:sale_deleted"`
		PaymentDeleted bool  `gorm:"column:payment_deleted"`
		ItemDeleted    bool  `gorm:"column:item_deleted"`
		SaleCents      int64 `gorm:"column:sale_cents"`
		Corrections    int64 `gorm:"column:corrections"`
	}
	if err := db.Raw(`SELECT (s.deleted_at IS NOT NULL) AS sale_deleted,
		(p.deleted_at IS NOT NULL) AS payment_deleted,(si.deleted_at IS NOT NULL) AS item_deleted,
		ROUND(s.total*100)::bigint AS sale_cents,
		(SELECT COUNT(*) FROM sale_corrections c WHERE c.gym_id=s.gym_id AND c.sale_id=s.id AND c.deleted_at IS NULL) AS corrections
		FROM sales s JOIN payments p ON p.gym_id=s.gym_id AND p.id=s.payment_id
		JOIN sale_items si ON si.gym_id=s.gym_id AND si.sale_id=s.id
		WHERE s.gym_id=? AND s.id=?`, gymID, saleID).Scan(&canonical).Error; err != nil {
		t.Fatal(err)
	}
	if !canonical.SaleDeleted || !canonical.PaymentDeleted || !canonical.ItemDeleted ||
		canonical.SaleCents != 0 || canonical.Corrections != 1 {
		t.Fatalf("canonical annul graph=%+v", canonical)
	}

	resurrectAt := annulledAt.Add(10 * time.Second)
	salePayload["version"], salePayload["updated_at"], salePayload["deleted_at"] = 3, resurrectAt.UnixMilli(), nil
	paymentPayload["version"], paymentPayload["updated_at"], paymentPayload["deleted_at"] = 3, resurrectAt.UnixMilli(), nil
	saleItemPayload["version"], saleItemPayload["updated_at"], saleItemPayload["deleted_at"] = 3, resurrectAt.UnixMilli(), nil
	late := PushRequest{ClientID: uuid.NewString(), ClientNow: resurrectAt, SchemaVersion: SchemaVersion,
		Batch: []PushItem{
			makePushItem("sales", saleID, 3, salePayload),
			makePushItem("payments", paymentID, 3, paymentPayload),
			makePushItem("sale_items", saleItemID, 3, saleItemPayload),
		}}
	lateResponse, status, err := performSyncPush(router, token, late)
	if err != nil || status != http.StatusOK || len(lateResponse.Results) != len(late.Batch) {
		t.Fatalf("resurrection graph status=%d err=%v response=%+v", status, err, lateResponse)
	}
	for _, result := range lateResponse.Results {
		if result.Status != StatusRejectedFinancialConflict {
			t.Fatalf("resurrection row=%+v, want financial conflict", result)
		}
	}
}

func TestAcceptedCorrectionRejectsLateIncompatibleSaleAndPayment(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	paymentID, saleID, correctionID := uuid.New(), uuid.New(), uuid.New()
	productID, saleItemID := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO products(
		id,gym_id,version,created_at,updated_at,name,price,stock,stock_minimum,active)
		VALUES (?, ?, 1, ?, ?, 'Producto corregido', 50, 10, 1, TRUE)`,
		productID, gymID, now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO payments(
		id,gym_id,version,created_at,updated_at,folio,amount,recognized_amount,
		payment_method,concept,discount_amount,balance_pending,payment_date,operator_id)
		VALUES (?, ?, 2, ?, ?, 'PRD-SYNC-CORR-WINNER', 40, 40, 'transfer', 'product', 10, 0, ?, ?)`,
		paymentID, gymID, now, now, now, userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sales(
		id,gym_id,version,created_at,updated_at,payment_id,subtotal,discount,total,correction_version)
		VALUES (?, ?, 2, ?, ?, ?, 50, 10, 40, 1)`,
		saleID, gymID, now, now, paymentID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO sale_items(
		id,gym_id,version,created_at,updated_at,sale_id,product_id,product_name_snapshot,
		unit_price_snapshot,unit_cost_snapshot,quantity,line_total)
		VALUES (?, ?, 2, ?, ?, ?, ?, 'Producto corregido', 50, 20, 1, 50)`,
		saleItemID, gymID, now, now, saleID, productID).Error; err != nil {
		t.Fatal(err)
	}
	lineSnapshot := map[string]any{"sale_item_id": saleItemID.String(), "product_id": productID.String(),
		"product_name": "Producto corregido", "unit_price_cents": 5000, "unit_cost_cents": 2000,
		"quantity": 1, "line_total_cents": 5000}
	correctionPayload, err := json.Marshal(map[string]any{
		"id": correctionID.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil,
		"sale_id": saleID.String(), "expected_sale_version": 0,
		"reason": "Corrección ganadora", "correction_type": "edit", "money_resolution": "record_only",
		"increase_resolution": nil, "monetary_delta": 10,
		"before_snapshot": map[string]any{"sale_id": saleID.String(), "correction_version": 0,
			"subtotal_cents": 5000, "discount_cents": 0, "total_cents": 5000,
			"payment_amount_cents": 5000, "recognized_amount_cents": 5000,
			"lines": []any{lineSnapshot}},
		"after_snapshot": map[string]any{"sale_id": saleID.String(), "correction_version": 1,
			"subtotal_cents": 5000, "discount_cents": 1000, "total_cents": 4000,
			"payment_amount_cents": 4000, "recognized_amount_cents": 4000,
			"lines": []any{lineSnapshot}},
		"idempotency_key": "correction:winner", "idempotency_fingerprint": "fingerprint:winner",
		"idempotency_result": map[string]any{"correction_id": correctionID.String()}, "created_by": userID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	winner := PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion,
		Batch: []PushItem{{QueueID: uuid.NewString(), EntityType: "sale_corrections", EntityID: correctionID.String(),
			Operation: OpUpsertStr, ClientVersion: 1, Payload: correctionPayload, EnqueuedAt: now}}}
	winnerResponse, status, err := performSyncPush(router, token, winner)
	if err != nil || status != http.StatusOK || len(winnerResponse.Results) != 1 || winnerResponse.Results[0].Status != StatusAccepted {
		t.Fatalf("accept correction status=%d err=%v response=%+v", status, err, winnerResponse)
	}

	lateAt := now.Add(10 * time.Second)
	salePayload, _ := json.Marshal(map[string]any{
		"id": saleID.String(), "gym_id": gymID.String(), "version": 3,
		"created_at": now.UnixMilli(), "updated_at": lateAt.UnixMilli(), "deleted_at": nil,
		"payment_id": paymentID.String(), "member_id": nil,
		"subtotal": 50.0, "discount": 15.0, "total": 35.0, "correction_version": 1,
	})
	paymentPayload, _ := json.Marshal(map[string]any{
		"id": paymentID.String(), "gym_id": gymID.String(), "version": 3,
		"created_at": now.UnixMilli(), "updated_at": lateAt.UnixMilli(), "deleted_at": nil,
		"folio": "PRD-SYNC-CORR-WINNER", "amount": 35.0, "recognized_amount": 35.0,
		"payment_method": "transfer", "concept": "product", "discount_amount": 15.0,
		"balance_pending": 0, "payment_date": now.Format("2006-01-02"), "operator_id": userID.String(),
	})
	saleItemPayload, _ := json.Marshal(map[string]any{
		"id": saleItemID.String(), "gym_id": gymID.String(), "version": 3,
		"created_at": now.UnixMilli(), "updated_at": lateAt.UnixMilli(), "deleted_at": nil,
		"sale_id": saleID.String(), "product_id": productID.String(), "product_name_snapshot": "Producto corregido",
		"unit_price_snapshot": 50.0, "unit_cost_snapshot": 20.0, "quantity": 2, "line_total": 100.0,
	})
	late := PushRequest{ClientID: uuid.NewString(), ClientNow: lateAt, SchemaVersion: SchemaVersion,
		Batch: []PushItem{
			{QueueID: uuid.NewString(), EntityType: "sales", EntityID: saleID.String(), Operation: OpUpsertStr,
				ClientVersion: 3, Payload: salePayload, EnqueuedAt: lateAt},
			{QueueID: uuid.NewString(), EntityType: "payments", EntityID: paymentID.String(), Operation: OpUpsertStr,
				ClientVersion: 3, Payload: paymentPayload, EnqueuedAt: lateAt},
			{QueueID: uuid.NewString(), EntityType: "sale_items", EntityID: saleItemID.String(), Operation: OpUpsertStr,
				ClientVersion: 3, Payload: saleItemPayload, EnqueuedAt: lateAt},
		}}
	lateResponse, status, err := performSyncPush(router, token, late)
	if err != nil || status != http.StatusOK || len(lateResponse.Results) != 3 {
		t.Fatalf("late graph status=%d err=%v response=%+v", status, err, lateResponse)
	}
	for _, result := range lateResponse.Results {
		if result.Status != StatusRejectedFinancialConflict {
			t.Fatalf("late incompatible row=%+v, want financial conflict", result)
		}
	}
	var canonical struct {
		SaleCents       int64 `gorm:"column:sale_cents"`
		PaymentCents    int64 `gorm:"column:payment_cents"`
		RecognizedCents int64 `gorm:"column:recognized_cents"`
	}
	if err := db.Raw(`SELECT ROUND(s.total*100)::bigint AS sale_cents,
		ROUND(p.amount*100)::bigint AS payment_cents,
		ROUND(p.recognized_amount*100)::bigint AS recognized_cents
		FROM sales s JOIN payments p ON p.id=s.payment_id WHERE s.gym_id=? AND s.id=?`,
		gymID, saleID).Scan(&canonical).Error; err != nil {
		t.Fatal(err)
	}
	if canonical.SaleCents != 4000 || canonical.PaymentCents != 4000 || canonical.RecognizedCents != 4000 {
		t.Fatalf("canonical corrected graph=%+v, want all $40", canonical)
	}
}

// Pin del fix de clock skew (jul-2026): el check rechazaba por |drift|, así
// que cualquier cambio hecho OFFLINE por más de MaxClockSkew quedaba
// imposible de subir para siempre (la fila se atoraba en sync_queue con
// reintentos infinitos y last_error vacío) — rompiendo la promesa central
// del offline-first. Un updated_at viejo es la operación offline normal y
// el LWW lo resuelve solo; sólo el drift hacia el FUTURO (reloj adelantado
// que ganaría todos los conflictos) se rechaza, y ahora CON razón legible.
// Run with -tags 'server integration'.
func TestPush_ClockSkew_RechazaSoloFuturo(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	push := func(t *testing.T, updatedAt time.Time) PushItemResult {
		t.Helper()
		id := uuid.New()
		payload := map[string]any{
			"id":         id.String(),
			"gym_id":     gymID.String(),
			"version":    1,
			"created_at": updatedAt.UnixMilli(),
			"updated_at": updatedAt.UnixMilli(),
			"name":       "Skew " + id.String()[:8],
			"price":      18,
			"stock":      1,
			"active":     true,
		}
		pb, _ := json.Marshal(payload)
		req := PushRequest{
			ClientID: uuid.NewString(), ClientNow: time.Now(), SchemaVersion: 1,
			Batch: []PushItem{{
				QueueID: uuid.NewString(), EntityType: "products", EntityID: id.String(),
				Operation: OpUpsertStr, ClientVersion: 1, Payload: pb, EnqueuedAt: time.Now(),
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
		if len(pr.Results) != 1 {
			t.Fatalf("results: %+v", pr.Results)
		}
		return pr.Results[0]
	}

	// Cambio hecho hace una hora (venta capturada sin internet): ACEPTA.
	if res := push(t, time.Now().Add(-time.Hour)); res.Status != StatusAccepted {
		t.Errorf("payload viejo (offline normal) = %s (%s), want accepted", res.Status, res.Error)
	}
	// Reloj adelantado una hora: RECHAZA, con razón legible (antes viajaba
	// sin mensaje y el sidecar mostraba "Último error: Ninguno").
	res := push(t, time.Now().Add(time.Hour))
	if res.Status != StatusRejectedClockSkew {
		t.Errorf("payload futuro = %s, want rejected_clock_skew", res.Status)
	}
	if res.Error == "" {
		t.Errorf("el rechazo por skew debe traer razón legible, vino vacío")
	}
}
