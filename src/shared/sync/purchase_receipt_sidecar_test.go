//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	migrations "github.com/cuadra/cuadra-core/db_migrations"
	infraDB "github.com/cuadra/cuadra-core/infraestructure/db"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	gymRepos "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchase "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	repos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

type remoteFixture struct {
	db                        *sqlx.DB
	uow                       shared.UnitOfWork
	gym, user, product, order uuid.UUID
	now                       time.Time
}

func remoteSidecar(t *testing.T) *remoteFixture {
	t.Helper()
	f := &remoteFixture{gym: uuid.New(), user: uuid.New(), product: uuid.New(), order: uuid.New(), now: time.Now().UTC()}
	f.db, f.uow = freshSidecarDBWithGym(t, f.gym)
	_, err := f.db.Exec(`INSERT INTO users(id,gym_id,version,created_at,updated_at,email,password_hash,full_name,role,active)VALUES(?,?,1,?,?,?,'hash','Operador','operator',1)`, f.user, f.gym, f.now.UnixMilli(), f.now.UnixMilli(), f.user.String()+"@test.local")
	if err != nil {
		t.Fatal(err)
	}
	f.apply(t, f.productChange(1, 5), f.orderChange(1, 23.45))
	return f
}
func (f *remoteFixture) change(kind string, id uuid.UUID, version int, fields map[string]any) syncpkg.PullChange {
	fields["id"] = id
	fields["gym_id"] = f.gym
	fields["version"] = version
	fields["created_at"] = f.now.UnixMilli()
	fields["updated_at"] = f.now.UnixMilli()
	fields["deleted_at"] = nil
	payload, _ := json.Marshal(fields)
	return syncpkg.PullChange{EntityType: kind, EntityID: id.String(), Version: version, Payload: payload, ServerUpdatedAt: f.now.Add(time.Duration(version) * time.Second)}
}
func (f *remoteFixture) productChange(version, base int) syncpkg.PullChange {
	return f.change("products", f.product, version, map[string]any{"name": "Agua", "price": 40, "stock": max(0, base+10), "stock_base": base, "stock_minimum": 0, "active": true})
}
func (f *remoteFixture) orderChange(version int, cost float64) syncpkg.PullChange {
	return f.change("inventory_purchases", f.order, version, map[string]any{"stock_movement_id": nil, "product_id": f.product, "quantity": 10, "unit_cost": cost, "total_amount": cost * 10, "status": "paid", "paid_on": "2026-09-01", "payment_method": "transfer", "paid_from": "gym_fund", "cash_movement_id": nil, "idempotency_key": "remote-purchase:" + f.order.String(), "created_by": f.user})
}
func (f *remoteFixture) receipt() *purchase.Receipt {
	return &purchase.Receipt{ID: f.order, GymID: f.gym, PurchaseID: f.order, ProductID: f.product, ReceivedBy: f.user, Quantity: 10, UnitCost: 23.45, CreatedAt: f.now}
}
func (f *remoteFixture) receiptChange(t *testing.T) syncpkg.PullChange {
	payload, err := repos.ReceiptPayload(f.receipt())
	if err != nil {
		t.Fatal(err)
	}
	return syncpkg.PullChange{EntityType: "inventory_purchase_receipts", EntityID: f.order.String(), Version: 1, Payload: payload, ServerUpdatedAt: f.now.Add(time.Minute)}
}
func (f *remoteFixture) apply(t *testing.T, changes ...syncpkg.PullChange) {
	t.Helper()
	if err := syncpkg.ApplyPullPage(context.Background(), f.uow, changes, nil); err != nil {
		t.Fatal(err)
	}
}
func (f *remoteFixture) assertStock(t *testing.T, want int) {
	t.Helper()
	var stock int
	if err := f.db.Get(&stock, `SELECT stock FROM products WHERE id=?`, f.product); err != nil || stock != want {
		t.Fatalf("stock=%d want=%d err=%v", stock, want, err)
	}
}
func TestRemoteReceipt_OfflineRetryAndCloudPaymentCorrection(t *testing.T) {
	f := remoteSidecar(t)
	f.assertStock(t, 5)
	uc := prodApp.NewReceiveInventoryPurchase(repos.NewInventoryPurchaseSQLiteRepository(), repos.NewInventoryPurchaseReceiptSQLiteRepository(), f.uow, audit.NewSQLiteRecorder())
	in := prodApp.ReceiveInventoryPurchaseInput{GymID: f.gym, PurchaseID: f.order, ActorUserID: f.user, ActorRole: "operator", Quantity: 10}
	for range 3 {
		if _, err := uc.Execute(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	f.assertStock(t, 15)
	var events, stockWrites, paymentWrites, cost int
	for query, dest := range map[string]*int{`SELECT COUNT(*) FROM inventory_purchase_receipts`: &events, `SELECT COUNT(*) FROM sync_queue WHERE entity_type='products'`: &stockWrites, `SELECT COUNT(*) FROM sync_queue WHERE entity_type='inventory_purchases'`: &paymentWrites, `SELECT cost FROM stock_movements WHERE product_id='` + f.product.String() + `'`: &cost} {
		if err := f.db.Get(dest, query); err != nil {
			t.Fatal(err)
		}
	}
	if events != 1 || stockWrites != 0 || paymentWrites != 0 || cost != 2345 {
		t.Fatalf("events/products/payments/cost=%d/%d/%d/%d", events, stockWrites, paymentWrites, cost)
	}
	// A newer cloud payment and a product snapshot must preserve an unsent receipt.
	f.apply(t, f.productChange(8, 2), f.orderChange(4, 25.75), f.receiptChange(t), f.receiptChange(t))
	f.assertStock(t, 12)
	var amount, version int
	var status string
	if err := f.db.QueryRow(`SELECT total_amount,version,status FROM inventory_purchases WHERE id=?`, f.order).Scan(&amount, &version, &status); err != nil {
		t.Fatal(err)
	}
	if amount != 25750 || version != 4 || status != "paid" {
		t.Fatalf("payment changed: %d v%d %s", amount, version, status)
	}
	// A local financial correction is rejected and rolls back.
	err := f.uow.Command(context.Background(), func(tx shared.Transaction) error {
		p, e := repos.NewInventoryPurchaseSQLiteRepository().GetByID(tx, f.gym, f.order)
		if e != nil {
			return e
		}
		_ = p.ReopenPayment(f.now)
		_, e = repos.NewInventoryPurchaseSQLiteRepository().Update(tx, p, 4)
		return e
	})
	if err == nil {
		t.Fatal("desktop overwrote remote payment")
	}
}
func TestRemoteReceipt_SoldDownSnapshotBeforeReceiptAndCatalogEdit(t *testing.T) {
	f := remoteSidecar(t)
	f.apply(t, f.productChange(2, -8))
	f.assertStock(t, -8)
	// Catalog edit while the receipt is still in transit keeps the negative base.
	err := f.uow.Command(context.Background(), func(tx shared.Transaction) error {
		r := repos.NewProductSQLiteRepository()
		p, e := r.GetByID(tx, f.product)
		if e != nil {
			return e
		}
		if e = p.Update("Agua nueva", 41, 0, nil, nil, f.now); e != nil {
			return e
		}
		_, e = r.Update(tx, p)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	f.apply(t, f.receiptChange(t))
	f.assertStock(t, 2)
	// A completed delivery is immutable; an altered retry cannot change stock.
	altered := f.receipt()
	altered.Quantity = 11
	err = f.uow.Command(context.Background(), func(tx shared.Transaction) error {
		_, e := repos.NewInventoryPurchaseReceiptSQLiteRepository().Apply(tx, altered, false)
		return e
	})
	if err == nil {
		t.Fatal("changed receipt accepted")
	}
	f.assertStock(t, 2)
}

func TestRemotePurchase_LocalFinancialActionsReturnBusinessErrorAndRollback(t *testing.T) {
	f := remoteSidecar(t)
	purchases := repos.NewInventoryPurchaseSQLiteRepository()
	products := repos.NewProductSQLiteRepository()
	cost := 30.0
	for name, run := range map[string]func() error{
		"reopen": func() error {
			_, err := prodApp.NewReopenInventoryPurchase(purchases, products, nil, f.uow, audit.NewSQLiteRecorder()).Execute(context.Background(), prodApp.ReopenInventoryPurchaseInput{GymID: f.gym, PurchaseID: f.order, ActorUserID: f.user, ActorRole: "owner", ExpectedVersion: 1, CorrectionReason: "No corresponde editar aquí"})
			return err
		},
		"correct": func() error {
			_, err := prodApp.NewCorrectInventoryPurchase(purchases, products, repos.NewStockMovementSQLiteRepository(), f.uow, audit.NewSQLiteRecorder()).Execute(context.Background(), prodApp.CorrectInventoryPurchaseInput{GymID: f.gym, PurchaseID: f.order, ActorUserID: f.user, ActorRole: "owner", ExpectedVersion: 1, Quantity: 10, UnitCost: &cost, CorrectionReason: "No corresponde editar aquí", IdempotencyKey: uuid.NewString()})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			if !errors.Is(err, prodErrors.ErrRemotePurchaseCloudOnly) || utils.DomainErrorToHttpCode(err) != http.StatusUnprocessableEntity {
				t.Fatalf("expected clear cloud-only rejection, got %v", err)
			}
			var version, total, pending int
			var status string
			if err := f.db.QueryRow(`SELECT version,total_amount,status FROM inventory_purchases WHERE id=?`, f.order).Scan(&version, &total, &status); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Get(&pending, `SELECT COUNT(*) FROM sync_queue`); err != nil {
				t.Fatal(err)
			}
			if version != 1 || total != 23450 || status != "paid" || pending != 0 {
				t.Fatalf("rejection changed financial state: v%d total=%d status=%s pending=%d", version, total, status, pending)
			}
			f.assertStock(t, 5)
		})
	}
}

func TestRemotePurchase_HistoryIncludesLocalEvening_SQLite(t *testing.T) {
	f := remoteSidecar(t)
	if _, err := f.db.Exec(`UPDATE gyms SET timezone='America/Mexico_City' WHERE id=?`, f.gym); err != nil {
		t.Fatal(err)
	}
	evening, _ := time.Parse(time.RFC3339, "2026-09-07T05:59:59Z")
	if _, err := f.db.Exec(`UPDATE inventory_purchases SET created_at=? WHERE id=?`, evening.UnixMilli(), f.order); err != nil {
		t.Fatal(err)
	}
	list := prodApp.NewListInventoryPurchases(repos.NewInventoryPurchaseSQLiteRepository(), repos.NewProductSQLiteRepository(), nil, f.uow).WithGyms(gymRepos.NewGymSQLiteRepository())
	day, _ := time.Parse("2006-01-02", "2026-09-06")
	in := prodApp.ListInventoryPurchasesInput{GymID: f.gym, Status: "all", From: &day, To: &day}
	result, err := list.ExecuteList(context.Background(), in)
	if err != nil || result.Total != 1 || result.Items[0].RecordedOn != "2026-09-06" {
		t.Fatalf("local evening missing: result=%+v err=%v", result, err)
	}
	if _, err := f.db.Exec(`UPDATE inventory_purchases SET created_at=? WHERE id=?`, evening.Add(time.Second).UnixMilli(), f.order); err != nil {
		t.Fatal(err)
	}
	result, err = list.ExecuteList(context.Background(), in)
	if err != nil || result.Total != 0 {
		t.Fatalf("next local day leaked into history: result=%+v err=%v", result, err)
	}
}
func TestRemoteReceipt_ReceiptBeforeParentAndTransactionRollback(t *testing.T) {
	f := remoteSidecar(t)
	_, err := f.db.Exec(`DELETE FROM inventory_purchases WHERE id=?`, f.order)
	if err != nil {
		t.Fatal(err)
	}
	f.apply(t, f.receiptChange(t), f.orderChange(1, 23.45))
	f.assertStock(t, 15)
	// A crash after projection rolls back the receipt, journal and counter together.
	f2 := remoteSidecar(t)
	sentinel := errors.New("simulated crash")
	err = f2.uow.Command(context.Background(), func(tx shared.Transaction) error {
		if _, e := repos.NewInventoryPurchaseReceiptSQLiteRepository().Create(tx, f2.receipt()); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	f2.assertStock(t, 5)
	var n int
	_ = f2.db.Get(&n, `SELECT COUNT(*) FROM inventory_purchase_receipts`)
	if n != 0 {
		t.Fatal("receipt committed before stock")
	}
	foreign := f2.receipt()
	foreign.GymID = uuid.New()
	err = f2.uow.Command(context.Background(), func(tx shared.Transaction) error {
		_, e := repos.NewInventoryPurchaseReceiptSQLiteRepository().Apply(tx, foreign, false)
		return e
	})
	if err == nil {
		t.Fatal("cross-gym receipt accepted")
	}
	f2.assertStock(t, 5)
}

func TestRemoteReceipt_FullSyncResumeKeepsFirstWatermark(t *testing.T) {
	f := remoteSidecar(t)
	start := f.now.Add(-time.Minute).Truncate(time.Millisecond)
	finished := f.now.Add(time.Minute)
	secondAttempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tinta-Sync-Schema") != strconv.Itoa(syncpkg.SchemaVersion) {
			t.Error("missing schema capability")
		}
		if r.URL.Path == "/api/v1/sync/full" {
			if r.URL.Query().Get("cursor") == "" {
				_ = json.NewEncoder(w).Encode(syncpkg.FullSyncResponse{ServerNow: start, SchemaVersion: 3, Changes: []syncpkg.PullChange{f.productChange(1, 5)}, HasMore: true, NextCursor: "second"})
				return
			}
			secondAttempts++
			if secondAttempts == 1 {
				http.Error(w, "temporary connection failure", 503)
				return
			}
			_ = json.NewEncoder(w).Encode(syncpkg.FullSyncResponse{ServerNow: finished, SchemaVersion: 3})
			return
		}
		since, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("since"))
		if err != nil || since.After(start) {
			t.Errorf("incremental skipped changes made during full sync: %s %v", since, err)
		}
		_ = json.NewEncoder(w).Encode(syncpkg.PullResponse{ServerNow: finished, SchemaVersion: 3, Changes: []syncpkg.PullChange{f.productChange(3, 2)}})
	}))
	defer srv.Close()
	makeAgent := func() *syncpkg.Agent {
		a := syncpkg.NewAgent(syncpkg.AgentConfig{BaseURL: srv.URL, HTTPClient: srv.Client()}, f.db, f.uow)
		a.SetToken("test")
		if err := a.Bootstrap(context.Background()); err != nil {
			t.Fatal(err)
		}
		return a
	}
	first := makeAgent()
	if err := first.FullSync(context.Background()); err == nil {
		t.Fatal("expected interrupted full sync")
	}
	resumed := makeAgent()
	if err := resumed.FullSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !resumed.Snapshot().LastPulledAt.Equal(start) {
		t.Fatalf("watermark=%s want first page %s", resumed.Snapshot().LastPulledAt, start)
	}
	if err := resumed.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.assertStock(t, 2)
}

func TestRemotePurchaseReports_SQLite(t *testing.T) {
	f := remoteSidecar(t)
	ctx := context.Background()
	tx, _ := f.uow.Query(ctx)
	day, _ := time.Parse("2006-01-02", "2026-09-01")
	reader := reportsInfra.NewSQLiteReader()
	total, err := reader.SumInventoryCostBetween(tx, f.gym, "America/Mexico_City", day, day)
	if err != nil || total != 234.5 {
		t.Fatalf("paid total before receipt=%v %v", total, err)
	}
	rows, err := reader.ListInventoryCostsBetween(tx, f.gym, "America/Mexico_City", day, day, 200)
	if err != nil || len(rows) != 1 || rows[0].MovementID != f.order || rows[0].CostTotal != 234.5 {
		t.Fatalf("paid rows before receipt=%+v %v", rows, err)
	}
	f.apply(t, f.receiptChange(t))
	tx, _ = f.uow.Query(ctx)
	total, err = reader.SumInventoryCostBetween(tx, f.gym, "America/Mexico_City", day, day)
	if err != nil || total != 234.5 {
		t.Fatalf("receipt double-counted payment=%v %v", total, err)
	}
}

func TestRemotePurchaseMigration_PreservesPaidRowsAndUnsentStock(t *testing.T) {
	gym, user, productID, movementID, purchaseID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	db, _ := freshSidecarDBWithGym(t, gym, "043_payment_cash_destination.sql")
	now := time.Now().UTC().UnixMilli()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,gym_id,created_at,updated_at,email,password_hash,full_name,role,active) VALUES(?,?,?,?,?,'hash','Owner','owner',1)`, []any{user, gym, now, now, user.String() + "@test.local"}},
		{`INSERT INTO products(id,gym_id,created_at,updated_at,name,price,stock,active) VALUES(?,?,?,?,'Agua',1500,4,1)`, []any{productID, gym, now, now}},
		{`INSERT INTO stock_movements(id,gym_id,created_at,updated_at,product_id,movement_type,delta,cost,is_purchase,operator_id) VALUES(?,?,?,?,?,'restock',4,850,1,?)`, []any{movementID, gym, now, now, productID, user}},
		{`INSERT INTO inventory_purchases(id,gym_id,created_at,updated_at,stock_movement_id,product_id,quantity,unit_cost,total_amount,status,paid_on,payment_method,paid_from,idempotency_key,created_by) VALUES(?,?,?,?,?,?,4,850,3400,'paid','2026-09-01','transfer','gym_fund','existing-purchase',?)`, []any{purchaseID, gym, now, now, movementID, productID, user}},
		{`INSERT INTO sync_queue(id,entity_type,entity_id,operation,payload,client_version,enqueued_at) VALUES(?,'products',?,'upsert','{"stock":4,"price":15,"name":"Agua"}',1,?)`, []any{uuid.New(), productID, now}},
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := infraDB.ApplySQLiteMigrations(db, migrations.SQLite, "sqlite"); err != nil {
			t.Fatal(err)
		}
	}
	var cost, amount, qty, base int
	var link, status string
	if err := db.QueryRow(`SELECT stock_movement_id,quantity,unit_cost,total_amount,status FROM inventory_purchases WHERE id=?`, purchaseID).Scan(&link, &qty, &cost, &amount, &status); err != nil {
		t.Fatal(err)
	}
	if link != movementID.String() || qty != 4 || cost != 850 || amount != 3400 || status != "paid" {
		t.Fatalf("purchase changed on upgrade: %s %d %d %d %s", link, qty, cost, amount, status)
	}
	if err := db.Get(&base, `SELECT json_extract(payload,'$.stock_base') FROM sync_queue WHERE entity_id=?`, productID); err != nil || base != 4 {
		t.Fatalf("pending stock=%d %v", base, err)
	}
	var violations []map[string]any
	rows, err := db.Queryx(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		row := map[string]any{}
		_ = rows.MapScan(row)
		violations = append(violations, row)
	}
	if len(violations) > 0 {
		t.Fatalf("broken foreign keys: %+v", violations)
	}
}
