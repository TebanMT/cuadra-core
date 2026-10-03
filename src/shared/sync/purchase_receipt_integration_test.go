//go:build server && integration

package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	gymRepos "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	product "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	purchase "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	repos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

type remotePG struct {
	db                        *gorm.DB
	uow                       shared.UnitOfWork
	gym, user, product, order uuid.UUID
	purchase                  *purchase.Purchase
}

func newRemotePG(t *testing.T) *remotePG {
	t.Helper()
	db := projectorTestDB(t)
	gym, user := seedGymAndOwner(t, db)
	f := &remotePG{db: db, uow: shared.NewPostgresUnitOfWork(db), gym: gym, user: user, product: uuid.New(), order: uuid.New()}
	t.Cleanup(func() { db.Exec(`DELETE FROM inventory_purchase_receipts WHERE gym_id=?`, gym) })
	err := f.uow.Command(context.Background(), func(tx shared.Transaction) error {
		p, e := product.New(f.product, gym, "Agua", 40, 5, 0, nil, nil, time.Now().UTC())
		if e != nil {
			return e
		}
		_, e = repos.NewProductPostgresRepository().Create(tx, p)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	uc := prodApp.NewCreateRemoteInventoryPurchase(repos.NewInventoryPurchasePostgresRepository(), repos.NewProductPostgresRepository(), gymRepos.NewGymPostgresRepository(), f.uow, audit.NewPostgresRecorder())
	day, _ := time.Parse("2006-01-02", "2026-09-01")
	in := prodApp.CreateRemoteInventoryPurchaseInput{ID: f.order, GymID: gym, ProductID: f.product, ActorUserID: user, ActorRole: "owner", Quantity: 10, UnitCost: 23.45, PaidOn: day, PaymentMethod: "transfer", PaidFrom: "gym_fund"}
	for range 2 {
		view, err := uc.Execute(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		f.purchase = view.Purchase
	}
	in.UnitCost = 24
	if _, err = uc.Execute(context.Background(), in); err == nil {
		t.Fatal("changed creation retry accepted")
	}
	f.assertStock(t, 5)
	var payments int64
	db.Table("inventory_purchases").Where("gym_id=?", gym).Count(&payments)
	if payments != 1 {
		t.Fatalf("payments=%d", payments)
	}
	return f
}
func (f *remotePG) assertStock(t *testing.T, want int) {
	t.Helper()
	var stock int
	if err := f.db.Raw(`SELECT stock FROM products WHERE id=?`, f.product).Scan(&stock).Error; err != nil || stock != want {
		t.Fatalf("stock=%d want=%d err=%v", stock, want, err)
	}
}
func (f *remotePG) receiptItem(t *testing.T) PushItem {
	t.Helper()
	receipt, err := purchase.NewReceipt(f.purchase, f.user, 10, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := repos.ReceiptPayload(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return PushItem{QueueID: uuid.NewString(), EntityID: f.order.String(), EntityType: "inventory_purchase_receipts", Operation: "upsert", ClientVersion: 1, Payload: payload}
}

func TestRemotePurchase_LateCommitRemainsVisibleAfterCursorAdvances(t *testing.T) {
	f := newRemotePG(t)
	ctx := context.Background()
	store := NewPostgresStore()
	slow, err := f.uow.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer f.uow.Rollback(slow)
	if _, err = store.UpsertOne(ctx, slow, f.gym, f.receiptItem(t)); err != nil {
		t.Fatal(err)
	}
	// A receipt is written but its transaction has not committed. A different
	// financial update commits first, and the desktop advances past that row.
	if err = f.uow.Command(ctx, func(tx shared.Transaction) error {
		return tx.(*shared.GormTransaction).Tx.Exec(`UPDATE sync_entities SET version=version+1,server_updated_at=clock_timestamp() WHERE gym_id=? AND entity_type='inventory_purchases' AND entity_id=?`, f.gym, f.order).Error
	}); err != nil {
		t.Fatal(err)
	}
	query, _ := f.uow.Query(ctx)
	first, _, err := store.ListSince(ctx, query, f.gym, time.Time{}, 1000)
	if err != nil || len(first) == 0 {
		t.Fatalf("first pull: %v %v", first, err)
	}
	for _, change := range first {
		if change.EntityType == "inventory_purchase_receipts" {
			t.Fatal("uncommitted receipt leaked")
		}
	}
	last := first[len(first)-1]
	// This also models a full download that started while the write was pending.
	fullAnchor := time.Now().UTC()
	if err = f.uow.Commit(slow); err != nil {
		t.Fatal(err)
	}
	changes, _, err := store.ListSinceCursor(ctx, query, f.gym, FullCursor{After: last.ServerUpdatedAt, EntityID: last.EntityID, EntityType: last.EntityType}, 1000)
	if err != nil || len(changes) != 1 || changes[0].EntityType != "inventory_purchase_receipts" {
		t.Fatalf("late commit lost after incremental cursor: changes=%v err=%v", changes, err)
	}
	changes, _, err = store.ListSince(ctx, query, f.gym, fullAnchor, 1000)
	if err != nil || len(changes) != 1 || changes[0].EntityType != "inventory_purchase_receipts" {
		t.Fatalf("late commit lost after full download: changes=%v err=%v", changes, err)
	}
}

func TestRemotePurchase_ConcurrentReceiptRetriesDoNotDuplicateStockOrPayment(t *testing.T) {
	f := newRemotePG(t)
	item := f.receiptItem(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.uow.Command(context.Background(), func(tx shared.Transaction) error {
				_, err := NewPostgresStore().UpsertOne(context.Background(), tx, f.gym, item)
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.assertStock(t, 15)
	var receipts, movements int64
	f.db.Table("inventory_purchase_receipts").Where("gym_id=?", f.gym).Count(&receipts)
	f.db.Table("stock_movements").Where("gym_id=?", f.gym).Count(&movements)
	if receipts != 1 || movements != 1 {
		t.Fatalf("receipts/movements=%d/%d", receipts, movements)
	}
	var total float64
	f.db.Raw(`SELECT SUM(total_amount) FROM inventory_purchases WHERE gym_id=? AND status='paid'`, f.gym).Scan(&total)
	if total != 234.5 {
		t.Fatalf("total paid=%v", total)
	}
	// Cloud correction does not change physical receipt or cost history.
	cost := 24.75
	corrected, err := prodApp.NewCorrectInventoryPurchase(repos.NewInventoryPurchasePostgresRepository(), repos.NewProductPostgresRepository(), repos.NewStockMovementPostgresRepository(), f.uow, audit.NewPostgresRecorder()).Execute(context.Background(), prodApp.CorrectInventoryPurchaseInput{GymID: f.gym, ActorUserID: f.user, ActorRole: "owner", PurchaseID: f.order, ExpectedVersion: 1, Quantity: 12, UnitCost: &cost, CorrectionReason: "Cantidad del pago corregida", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if corrected.StockDelta != 0 || *corrected.TotalAmount != 297 {
		t.Fatalf("correction=%+v", corrected)
	}
	f.assertStock(t, 15)
	// A stale local financial snapshot cannot replace the cloud correction,
	// even with an artificially larger version/timestamp.
	var raw map[string]any
	var falsePurchases int64
	if err := f.db.Table("stock_movements").Where("gym_id=? AND delta=0 AND is_purchase=TRUE", f.gym).Count(&falsePurchases).Error; err != nil || falsePurchases != 0 {
		t.Fatalf("financial correction became a second purchase: count=%d err=%v", falsePurchases, err)
	}
	row := struct{ Payload []byte }{}
	f.db.Raw(`SELECT payload FROM sync_entities WHERE gym_id=? AND entity_type='inventory_purchases' AND entity_id=?`, f.gym, f.order).Scan(&row)
	_ = json.Unmarshal(row.Payload, &raw)
	raw["version"] = 99
	raw["status"] = "unpaid"
	raw["paid_on"] = nil
	raw["payment_method"] = nil
	raw["paid_from"] = nil
	raw["updated_at"] = time.Now().UTC().UnixMilli()
	payload, _ := json.Marshal(raw)
	err = f.uow.Command(context.Background(), func(tx shared.Transaction) error {
		_, e := NewPostgresStore().UpsertOne(context.Background(), tx, f.gym, PushItem{EntityType: "inventory_purchases", EntityID: f.order.String(), Operation: "upsert", ClientVersion: 99, Payload: payload})
		return e
	})
	if err == nil {
		t.Fatal("stale payment won sync")
	}
	f.assertStock(t, 15)
	// Both feeds publish the receipt, preserving pesos, without an independently
	// synchronizable derived stock event.
	tx, _ := f.uow.Query(context.Background())
	for _, full := range []bool{false, true} {
		var changes []PullChange
		if full {
			changes, _, _, err = NewPostgresStore().ListForFullSync(context.Background(), tx, f.gym, FullCursor{}, 1000)
		} else {
			changes, _, err = NewPostgresStore().ListSince(context.Background(), tx, f.gym, time.Time{}, 1000)
		}
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ch := range changes {
			if ch.EntityType == "inventory_purchase_receipts" {
				found = true
				r, e := repos.ReceiptFromPayload(ch.Payload)
				if e != nil || r.UnitCost != 23.45 {
					t.Fatalf("wire=%s %v", ch.Payload, e)
				}
			}
		}
		if !found {
			t.Fatalf("missing receipt full=%v", full)
		}
	}
}
func TestRemotePurchase_SnapshotReceiptRaceAndTenantGuard(t *testing.T) {
	f := newRemotePG(t)
	item := f.receiptItem(t)
	raw := map[string]any{"id": f.product, "gym_id": f.gym, "version": 2, "created_at": time.Now().UnixMilli(), "updated_at": time.Now().UnixMilli(), "name": "Agua nueva", "price": 40, "stock": 2, "stock_base": -8, "stock_minimum": 0, "active": true}
	payload, _ := json.Marshal(raw)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, push := range []PushItem{item, {EntityType: "products", EntityID: f.product.String(), Operation: "upsert", ClientVersion: 2, Payload: payload}} {
		wg.Add(1)
		go func(p PushItem) {
			defer wg.Done()
			errs <- f.uow.Command(context.Background(), func(tx shared.Transaction) error {
				_, e := NewPostgresStore().UpsertOne(context.Background(), tx, f.gym, p)
				return e
			})
		}(push)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.assertStock(t, 2)
	// A receipt cannot be reassigned, deleted, or change units after acceptance.
	var receipt map[string]any
	_ = json.Unmarshal(item.Payload, &receipt)
	receipt["quantity"] = 11
	altered, _ := json.Marshal(receipt)
	item.Payload = altered
	err := f.uow.Command(context.Background(), func(tx shared.Transaction) error {
		_, e := NewPostgresStore().UpsertOne(context.Background(), tx, f.gym, item)
		return e
	})
	if err == nil {
		t.Fatal("changed delivery accepted")
	}
	f.assertStock(t, 2)
	tx, _ := f.uow.Begin(context.Background())
	defer f.uow.Rollback(tx)
	result, err := NewPostgresStore().UpsertOne(context.Background(), tx, uuid.New(), item)
	if err != nil || result.Status != StatusRejectedUnauthorized {
		t.Fatalf("tenant guard=%+v %v", result, err)
	}
}
func TestRemotePurchase_RejectsOldReadersAndWriters(t *testing.T) {
	f := newRemotePG(t)
	router, tokens := newRealHandler(t, f.db)
	token, _ := tokens.GenerateAccessToken(f.user, f.gym, "owner")
	for _, path := range []string{"/api/v1/sync/pull", "/api/v1/sync/full"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUpgradeRequired {
			t.Fatalf("old %s status=%d %s", path, w.Code, w.Body.String())
		}
		req = httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Tinta-Sync-Schema", "3")
		w = httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("new %s status=%d %s", path, w.Code, w.Body.String())
		}
	}
	response := doJSONReq(t, router, "POST", "/api/v1/sync/push", token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: 2, Batch: []PushItem{f.receiptItem(t)}})
	if response.Code != 426 {
		t.Fatalf("old push=%d %s", response.Code, response.Body.String())
	}
}

func TestRemotePurchase_IncrementalCursorKeepsTimestampAndIDTies(t *testing.T) {
	f := newRemotePG(t)
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	// IDs can coincide across entity types (a receipt has its purchase's ID).
	for _, kind := range []string{"expenses", "products"} {
		if err := f.db.Exec(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at) VALUES(?,?,?,1,'{}',?)`, f.gym, kind, f.order, stamp).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Simulate historical timestamps without the new commit stamp normalizing
	// them. This override is confined to this fixture's transaction.
	if err := f.db.Transaction(func(g *gorm.DB) error {
		if err := g.Exec(`SET LOCAL tinta.sync_stamping='on'`).Error; err != nil {
			return err
		}
		return g.Exec(`UPDATE sync_entities SET server_updated_at=? WHERE gym_id=?`, stamp, f.gym).Error
	}); err != nil {
		t.Fatal(err)
	}
	var expected int64
	f.db.Table("sync_entities").Where("gym_id=?", f.gym).Count(&expected)
	tx, _ := f.uow.Query(context.Background())
	cursor := FullCursor{}
	seen := map[string]bool{}
	for page := 0; page < 10; page++ {
		items, more, err := NewPostgresStore().ListSinceCursor(context.Background(), tx, f.gym, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("page %d length=%d", page, len(items))
		}
		last := items[0]
		key := last.EntityType + last.EntityID
		if seen[key] {
			t.Fatal("duplicate page", key)
		}
		seen[key] = true
		cursor = FullCursor{After: last.ServerUpdatedAt, EntityID: last.EntityID, EntityType: last.EntityType}
		if !more {
			break
		}
	}
	if int64(len(seen)) != expected {
		t.Fatalf("received %d/%d rows at identical timestamps", len(seen), expected)
	}
}

func TestRemotePurchase_HistoryIncludesLocalEvening_Postgres(t *testing.T) {
	f := newRemotePG(t)
	if err := f.db.Exec(`UPDATE inventory_purchases SET created_at='2026-09-07T05:59:59Z' WHERE id=?`, f.order).Error; err != nil {
		t.Fatal(err)
	}
	list := prodApp.NewListInventoryPurchases(repos.NewInventoryPurchasePostgresRepository(), repos.NewProductPostgresRepository(), nil, f.uow).WithGyms(gymRepos.NewGymPostgresRepository())
	day, _ := time.Parse("2006-01-02", "2026-09-06")
	in := prodApp.ListInventoryPurchasesInput{GymID: f.gym, Status: "all", From: &day, To: &day}
	result, err := list.ExecuteList(context.Background(), in)
	if err != nil || result.Total != 1 || result.Items[0].RecordedOn != "2026-09-06" {
		t.Fatalf("local evening missing: result=%+v err=%v", result, err)
	}
	if err := f.db.Exec(`UPDATE inventory_purchases SET created_at='2026-09-07T06:00:00Z' WHERE id=?`, f.order).Error; err != nil {
		t.Fatal(err)
	}
	result, err = list.ExecuteList(context.Background(), in)
	if err != nil || result.Total != 0 {
		t.Fatalf("next local day leaked into history: result=%+v err=%v", result, err)
	}
}

func TestRemotePurchaseReports_Postgres(t *testing.T) {
	f := newRemotePG(t)
	ctx := context.Background()
	tx, _ := f.uow.Query(ctx)
	day, _ := time.Parse("2006-01-02", "2026-09-01")
	reader := reportsInfra.NewPostgresReader()
	total, err := reader.SumInventoryCostBetween(tx, f.gym, "America/Mexico_City", day, day)
	if err != nil || total != 234.5 {
		t.Fatalf("paid total before receipt=%v %v", total, err)
	}
	rows, err := reader.ListInventoryCostsBetween(tx, f.gym, "America/Mexico_City", day, day, 200)
	if err != nil || len(rows) != 1 || rows[0].MovementID != f.order || rows[0].CostTotal != 234.5 {
		t.Fatalf("paid rows before receipt=%+v %v", rows, err)
	}
	if err = f.uow.Command(ctx, func(tx shared.Transaction) error {
		_, e := NewPostgresStore().UpsertOne(ctx, tx, f.gym, f.receiptItem(t))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	tx, _ = f.uow.Query(ctx)
	total, err = reader.SumInventoryCostBetween(tx, f.gym, "America/Mexico_City", day, day)
	if err != nil || total != 234.5 {
		t.Fatalf("receipt double-counted payment=%v %v", total, err)
	}
}

func TestRemotePurchase_TwoTerminalsWithDifferentCostSnapshotsShareReceipt(t *testing.T) {
	f := newRemotePG(t)
	item := f.receiptItem(t)
	ctx := context.Background()
	if err := f.uow.Command(ctx, func(tx shared.Transaction) error {
		_, e := NewPostgresStore().UpsertOne(ctx, tx, f.gym, item)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal(item.Payload, &raw)
	raw["unit_cost"] = 25.75
	raw["created_at"] = time.Now().UTC().UnixMilli()
	raw["updated_at"] = raw["created_at"]
	item.Payload, _ = json.Marshal(raw)
	err := f.uow.Command(ctx, func(tx shared.Transaction) error {
		result, e := NewPostgresStore().UpsertOne(ctx, tx, f.gym, item)
		if e != nil {
			return e
		}
		if result.Status != StatusConflictServerWins {
			t.Fatalf("canonical receipt not returned: %+v", result)
		}
		canonical, e := repos.ReceiptFromPayload(result.ServerPayload)
		if e != nil {
			return e
		}
		if canonical.UnitCost != 23.45 {
			t.Fatal("canonical valuation changed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.assertStock(t, 15)
}
