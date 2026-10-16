//go:build server && integration

package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func TestSyncRejectsOversizedBatchAndBody(t *testing.T) {
	db := projectorTestDB(t)
	gym, owner := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, _ := tokens.GenerateAccessToken(owner, gym, "owner")
	batch, _ := json.Marshal(PushRequest{SchemaVersion: SchemaVersion, Batch: make([]PushItem, 2001)})
	for _, body := range []string{string(batch), `{"client_id":"` + strings.Repeat("x", 8<<20) + `"}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/push", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized body: %d", response.Code)
		}
	}
}

// Exercise the real push adapter, journal and domain tables with two tenants.
func TestPushCannotMoveEntityBetweenGyms(t *testing.T) {
	db := projectorTestDB(t)
	a, owner := seedGymAndOwner(t, db)
	b, _ := seedGymAndOwner(t, db)
	id := uuid.New()
	if err := db.Exec(`INSERT INTO products(id,gym_id,name,price,stock,stock_minimum,active) VALUES (?,?,'Original',10,5,0,true)`, id, b).Error; err != nil {
		t.Fatal(err)
	}
	router, tokens := newRealHandler(t, db)
	token, _ := tokens.GenerateAccessToken(owner, a, "owner")
	payload, _ := json.Marshal(map[string]any{"id": id, "gym_id": a, "version": 1, "name": "Intruso", "price": 1, "stock": 0, "stock_minimum": 0, "active": true, "created_at": time.Now().UnixMilli(), "updated_at": time.Now().UnixMilli()})
	resp, status, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: []PushItem{{QueueID: uuid.NewString(), EntityType: "products", EntityID: id.String(), Operation: OpUpsertStr, ClientVersion: 1, Payload: payload}}})
	if err != nil || status != 200 {
		t.Fatalf("push: %d %v", status, err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Status != StatusRejectedUnauthorized {
		t.Errorf("foreign ID accepted: %+v", resp.Results)
	}
	var gym string
	if err := db.Raw(`SELECT gym_id FROM products WHERE id=?`, id).Scan(&gym).Error; err != nil {
		t.Fatal(err)
	}
	if gym != b.String() {
		t.Errorf("product moved from %s to %s", b, gym)
	}
	var count int64
	db.Raw(`SELECT COUNT(*) FROM sync_entities WHERE gym_id=? AND entity_id=?`, a, id).Scan(&count)
	if count != 0 {
		t.Errorf("rejected write left %d journal rows", count)
	}
}

func TestPushCannotReferenceAnotherGymsProduct(t *testing.T) {
	db := projectorTestDB(t)
	a, owner := seedGymAndOwner(t, db)
	b, _ := seedGymAndOwner(t, db)
	product, id := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO products(id,gym_id,name,price,stock,stock_minimum,active) VALUES (?,?,'Ajeno',10,5,0,true)`, product, b).Error; err != nil {
		t.Fatal(err)
	}
	router, tokens := newRealHandler(t, db)
	token, _ := tokens.GenerateAccessToken(owner, a, "owner")
	payload, _ := json.Marshal(map[string]any{"id": id, "gym_id": a, "version": 1, "product_id": product, "movement_type": "restock", "delta": 2, "operator_id": owner, "created_at": time.Now().UnixMilli(), "updated_at": time.Now().UnixMilli()})
	resp, _, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: []PushItem{{QueueID: uuid.NewString(), EntityType: "stock_movements", EntityID: id.String(), Operation: OpUpsertStr, ClientVersion: 1, Payload: payload}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Status != StatusRejectedUnauthorized {
		t.Fatalf("foreign reference accepted: %+v", resp.Results)
	}
}

// A transaction that writes early but commits late must not land behind a
// cursor already returned to a desktop. Wall-clock write timestamps alone
// do not provide that guarantee, even when clock_timestamp() replaces NOW().
func TestJournalCommitOrderCannotSkipDelayedTransaction(t *testing.T) {
	db := projectorTestDB(t)
	gym, _ := seedGymAndOwner(t, db)
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	ctx := context.Background()
	first, err := uow.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer uow.Rollback(first)
	a, b := uuid.New(), uuid.New()
	insert := func(tx sharedDomain.Transaction, id uuid.UUID) error {
		return gormTx(tx).Exec(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at) VALUES (?,'products',?,1,'{}',clock_timestamp())`, gym, id).Error
	}
	if err := insert(first, a); err != nil {
		t.Fatal(err)
	}
	started, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		finished <- uow.Command(ctx, func(tx sharedDomain.Transaction) error { return insert(tx, b) })
	}()
	<-started
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("independent write was blocked before commit")
	}
	var cursor time.Time
	if err := db.Raw(`SELECT server_updated_at FROM sync_entities WHERE gym_id=? AND entity_id=?`, gym, b).Scan(&cursor).Error; err != nil {
		t.Fatal(err)
	}
	if err := uow.Commit(first); err != nil {
		t.Fatal(err)
	}
	query, _ := uow.Query(ctx)
	changes, _, err := NewPostgresStore().ListSince(ctx, query, gym, cursor, 100)
	if err != nil || len(changes) != 1 || changes[0].EntityID != a.String() {
		t.Fatalf("late commit lost: changes=%v err=%v", changes, err)
	}
	var times []time.Time
	if err := db.Raw(`SELECT server_updated_at FROM sync_entities WHERE gym_id=? ORDER BY server_updated_at`, gym).Scan(&times).Error; err != nil {
		t.Fatal(err)
	}
	if len(times) != 2 || !times[1].After(times[0]) {
		t.Fatalf("journal timestamps not strictly ordered: %v", times)
	}
}

func TestStockSnapshotsCannotEraseIndependentMovements(t *testing.T) {
	db := projectorTestDB(t)
	gym, owner := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, _ := tokens.GenerateAccessToken(owner, gym, "owner")
	productID := uuid.New()
	push := func(kind string, id uuid.UUID, version int, values map[string]any) PushItemResult {
		t.Helper()
		values["id"], values["gym_id"], values["version"] = id, gym, version
		values["created_at"], values["updated_at"] = time.Now().UnixMilli(), time.Now().UnixMilli()
		payload, _ := json.Marshal(values)
		response, _, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: []PushItem{{QueueID: uuid.NewString(), EntityType: kind, EntityID: id.String(), Operation: OpUpsertStr, ClientVersion: version, Payload: payload}}})
		if err != nil || len(response.Results) != 1 {
			t.Fatalf("push: %v %+v", err, response)
		}
		return response.Results[0]
	}
	snapshot := func(stock int) map[string]any {
		return map[string]any{"name": "Agua concurrente", "price": 19.75, "stock": stock, "stock_base": stock, "stock_minimum": 0, "active": true}
	}
	movement := func(delta int) map[string]any {
		return map[string]any{"product_id": productID, "movement_type": "count_correction", "delta": delta, "operator_id": owner}
	}
	push("products", productID, 1, snapshot(10))
	push("stock_movements", uuid.New(), 1, movement(10))
	// Both devices read 10, then independently sell 2 and 3 while offline.
	push("products", productID, 2, snapshot(8))
	a := uuid.New()
	push("stock_movements", a, 1, movement(-2))
	push("products", productID, 2, snapshot(7))
	push("stock_movements", uuid.New(), 1, movement(-3))
	push("stock_movements", a, 1, movement(-2)) // response lost, retry
	var stock int
	if err := db.Raw(`SELECT stock FROM products WHERE id=?`, productID).Scan(&stock).Error; err != nil || stock != 5 {
		t.Fatalf("stock=%d want 5, err=%v", stock, err)
	}
	if result := push("stock_movements", a, 99, movement(-9)); result.Status != StatusRejectedFinancialConflict {
		t.Fatalf("rewrote movement: %+v", result)
	}
	if err := db.Raw(`SELECT stock FROM products WHERE id=?`, productID).Scan(&stock).Error; err != nil || stock != 5 {
		t.Fatalf("rejected rewrite changed stock=%d, err=%v", stock, err)
	}
}

func TestOfflinePaymentFolioCollisionPreservesBothPayments(t *testing.T) {
	db := projectorTestDB(t)
	gym, owner := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, _ := tokens.GenerateAccessToken(owner, gym, "owner")
	push := func(id uuid.UUID) PushItemResult {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"id": id, "gym_id": gym, "version": 1, "folio": "OTR-000001",
			"amount": 19.75, "recognized_amount": 19.75, "balance_pending": 0,
			"discount_amount": 0, "concept": "other", "payment_method": "transfer",
			"payment_date": "2026-10-14", "operator_id": owner,
			"created_at": time.Now().Add(-time.Hour).UnixMilli(), "updated_at": time.Now().Add(-time.Hour).UnixMilli(),
		})
		response, _, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: []PushItem{{QueueID: uuid.NewString(), EntityType: "payments", EntityID: id.String(), Operation: OpUpsertStr, ClientVersion: 1, Payload: payload}}})
		if err != nil || len(response.Results) != 1 {
			t.Fatalf("push: %v %+v", err, response)
		}
		return response.Results[0]
	}
	a, b := uuid.New(), uuid.New()
	if result := push(a); result.Status != StatusAccepted {
		t.Fatalf("first: %+v", result)
	}
	result := push(b)
	if result.Status != StatusConflictServerWins || result.ServerVersion <= 1 {
		t.Fatalf("canonical acknowledgment missing: %+v", result)
	}
	canonical := decodePushPayload(result.ServerPayload)
	if canonical["folio"] != "OTR-000002" {
		t.Fatalf("canonical: %v", canonical)
	}
	push(b) // replay after losing the commit response
	var count, cents, versions int64
	db.Raw(`SELECT COUNT(*) FROM payments WHERE gym_id=?`, gym).Scan(&count)
	db.Raw(`SELECT ROUND(SUM(amount)*100)::bigint FROM payments WHERE gym_id=?`, gym).Scan(&cents)
	db.Raw(`SELECT COUNT(*) FROM payments p JOIN sync_entities s ON s.gym_id=p.gym_id AND s.entity_id=p.id AND s.entity_type='payments' WHERE p.gym_id=? AND p.version<>s.version`, gym).Scan(&versions)
	if count != 2 || cents != 3950 || versions != 0 {
		t.Fatalf("count=%d cents=%d version mismatches=%d", count, cents, versions)
	}
}
