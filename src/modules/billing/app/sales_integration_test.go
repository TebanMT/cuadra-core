//go:build sidecar

package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	billingRepoLite "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	prodRepoLite "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
)

// salesFixture extends the base billingFixture (in billing_integration_test.go)
// with the products + sales repos needed for UC-025/27.
type salesFixture struct {
	*billingFixture
	saleRepo          *billingRepoLite.SaleSQLiteRepository
	saleItemRepo      *billingRepoLite.SaleItemSQLiteRepository
	productRepo       *prodRepoLite.ProductSQLiteRepository
	stockMovementRepo *prodRepoLite.StockMovementSQLiteRepository
	productSvc        *prodApp.ProductService
	cashCloseReader   *billingRepoLite.CashCloseSQLiteReader
	cashCloseEvents   *billingRepoLite.CashCloseEventSQLiteRepository
}

func setupSales(t *testing.T) *salesFixture {
	base := setup(t)
	productRepo := prodRepoLite.NewProductSQLiteRepository()
	stockMovementRepo := prodRepoLite.NewStockMovementSQLiteRepository()
	return &salesFixture{
		billingFixture:    base,
		saleRepo:          billingRepoLite.NewSaleSQLiteRepository(),
		saleItemRepo:      billingRepoLite.NewSaleItemSQLiteRepository(),
		productRepo:       productRepo,
		stockMovementRepo: stockMovementRepo,
		productSvc:        prodApp.NewProductService(productRepo, stockMovementRepo),
		cashCloseReader:   billingRepoLite.NewCashCloseSQLiteReader(),
		cashCloseEvents:   billingRepoLite.NewCashCloseEventSQLiteRepository(),
	}
}

func (f *salesFixture) registerSale() *billingApp.RegisterSale {
	return billingApp.NewRegisterSale(
		f.paymentRepo, f.saleRepo, f.saleItemRepo, f.folios,
		f.productSvc, f.memberRepo, f.uow, f.recorder, billingApp.NoopPublisher{},
	)
}

func (f *salesFixture) seedProduct(t *testing.T, name string, price float64, stock int) uuid.UUID {
	t.Helper()
	uc := prodApp.NewCreateProduct(f.productRepo, f.stockMovementRepo, f.uow, f.recorder)
	out, err := uc.Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Name: name, Price: price, InitialStock: stock,
	})
	if err != nil {
		t.Fatalf("seed product %s: %v", name, err)
	}
	return out.ProductID
}

// ---------------------------------------------------------------------------
// UC-025 — RegisterSale (the hot path of Sesión 4)
// ---------------------------------------------------------------------------

func TestUC025_HappyPath_AtomicWrites(t *testing.T) {
	f := setupSales(t)
	pAgua := f.seedProduct(t, "Agua", 20, 24)
	pGato := f.seedProduct(t, "Gatorade", 25, 15)

	uc := f.registerSale()
	out, err := uc.Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{
			{ProductID: pAgua, Quantity: 2},
			{ProductID: pGato, Quantity: 1},
		},
	})
	if err != nil {
		t.Fatalf("sale: %v", err)
	}
	if out.Total != 65 {
		t.Errorf("total = %v, want 65", out.Total)
	}
	if out.Folio == "" || out.Folio[:4] != "PRD-" {
		t.Errorf("folio = %q, want PRD-*", out.Folio)
	}

	// 1 payment + 1 sale + 2 sale_items + 2 sale stock_movements.
	var nP, nS, nSI, nSM int
	_ = f.db.Get(&nP, "SELECT COUNT(*) FROM payments WHERE concept='product'")
	_ = f.db.Get(&nS, "SELECT COUNT(*) FROM sales")
	_ = f.db.Get(&nSI, "SELECT COUNT(*) FROM sale_items")
	_ = f.db.Get(&nSM, "SELECT COUNT(*) FROM stock_movements WHERE movement_type='sale'")
	if nP != 1 || nS != 1 || nSI != 2 || nSM != 2 {
		t.Errorf("counts payment=%d sale=%d items=%d movements=%d (want 1/1/2/2)", nP, nS, nSI, nSM)
	}

	// Stock decremented on both products.
	var aguaStock, gatoStock int
	_ = f.db.Get(&aguaStock, "SELECT stock FROM products WHERE id=?", pAgua.String())
	_ = f.db.Get(&gatoStock, "SELECT stock FROM products WHERE id=?", pGato.String())
	if aguaStock != 22 || gatoStock != 14 {
		t.Errorf("stock = agua %d gato %d (want 22/14)", aguaStock, gatoStock)
	}

	// Payment amount stored as cents.
	var payAmount int64
	_ = f.db.Get(&payAmount, "SELECT amount FROM payments WHERE id=?", out.PaymentID.String())
	if payAmount != 6500 {
		t.Errorf("payment amount cents = %d, want 6500", payAmount)
	}

	// Snapshot fields persisted on each sale_item.
	var snap struct {
		Name  string `db:"product_name_snapshot"`
		Price int64  `db:"unit_price_snapshot"`
	}
	_ = f.db.Get(&snap, "SELECT product_name_snapshot, unit_price_snapshot FROM sale_items WHERE product_id=?", pAgua.String())
	if snap.Name != "Agua" || snap.Price != 2000 {
		t.Errorf("snapshot agua: name=%q price=%d", snap.Name, snap.Price)
	}
}

func TestUC025_OversellRollsBackEverything_DA25_2(t *testing.T) {
	f := setupSales(t)
	pPlenty := f.seedProduct(t, "Plenty", 10, 50)
	pScarce := f.seedProduct(t, "Scarce", 30, 1)

	uc := f.registerSale()
	_, err := uc.Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{
			{ProductID: pPlenty, Quantity: 5}, // would succeed alone
			{ProductID: pScarce, Quantity: 5}, // forces a fail mid-cart
		},
	})
	if err == nil {
		t.Fatalf("oversell sale should fail")
	}

	// Atomicity: NOTHING persisted — no payment, no sale, no sale_items, no movements.
	for _, q := range []string{
		"SELECT COUNT(*) FROM payments WHERE concept='product'",
		"SELECT COUNT(*) FROM sales",
		"SELECT COUNT(*) FROM sale_items",
		"SELECT COUNT(*) FROM stock_movements WHERE movement_type='sale'",
	} {
		var n int
		_ = f.db.Get(&n, q)
		if n != 0 {
			t.Errorf("rollback failure (%s): n=%d", q, n)
		}
	}
	// First product's stock not touched.
	var s int
	_ = f.db.Get(&s, "SELECT stock FROM products WHERE id=?", pPlenty.String())
	if s != 50 {
		t.Errorf("stock mutated despite rollback: %d", s)
	}
}

func TestUC025_EmptyCartRejected(t *testing.T) {
	f := setupSales(t)
	uc := f.registerSale()
	if _, err := uc.Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
	}); err == nil {
		t.Errorf("empty cart should be rejected")
	}
}

func TestUC025_AnonymousSaleAllowed_DA25_3(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Agua", 20, 10)
	uc := f.registerSale()
	out, err := uc.Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		MemberID: nil,
		Items:    []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("anon sale: %v", err)
	}
	// Sale row member_id should be NULL.
	var memberID *string
	_ = f.db.Get(&memberID, "SELECT member_id FROM sales WHERE id=?", out.SaleID.String())
	if memberID != nil {
		t.Errorf("anon sale should have NULL member, got %v", *memberID)
	}
}

func TestUC025_LinkedToMember(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Agua", 20, 10)
	uc := f.registerSale()
	mid := f.memberID
	out, err := uc.Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "transfer",
		MemberID: &mid,
		Items:    []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("linked sale: %v", err)
	}
	var memberID *string
	_ = f.db.Get(&memberID, "SELECT member_id FROM sales WHERE id=?", out.SaleID.String())
	if memberID == nil || *memberID != mid.String() {
		t.Errorf("expected member %s, got %v", mid, memberID)
	}
}

// Property: vending the same set of items in different orders should yield
// the same final stock and same total.
func TestUC025_OrderIndependent_SameFinalState(t *testing.T) {
	f1 := setupSales(t)
	pA1 := f1.seedProduct(t, "Alfa", 10, 100)
	pB1 := f1.seedProduct(t, "Beta", 20, 100)
	pC1 := f1.seedProduct(t, "Gama", 30, 100)

	uc1 := f1.registerSale()
	for _, items := range [][]billingApp.SaleLineInput{
		{{ProductID: pA1, Quantity: 2}, {ProductID: pB1, Quantity: 3}},
		{{ProductID: pC1, Quantity: 1}, {ProductID: pA1, Quantity: 1}},
		{{ProductID: pB1, Quantity: 2}},
	} {
		if _, err := uc1.Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f1.gymID, ActorUserID: f1.ownerID, Method: "cash", Items: items,
		}); err != nil {
			t.Fatalf("order1 sale: %v", err)
		}
	}
	var aA, aB, aC, totalA int64
	_ = f1.db.Get(&aA, "SELECT stock FROM products WHERE id=?", pA1.String())
	_ = f1.db.Get(&aB, "SELECT stock FROM products WHERE id=?", pB1.String())
	_ = f1.db.Get(&aC, "SELECT stock FROM products WHERE id=?", pC1.String())
	_ = f1.db.Get(&totalA, "SELECT COALESCE(SUM(amount),0) FROM payments WHERE concept='product'")

	// Second run: shuffle the order completely.
	f2 := setupSales(t)
	pA2 := f2.seedProduct(t, "Alfa", 10, 100)
	pB2 := f2.seedProduct(t, "Beta", 20, 100)
	pC2 := f2.seedProduct(t, "Gama", 30, 100)
	uc2 := f2.registerSale()
	for _, items := range [][]billingApp.SaleLineInput{
		{{ProductID: pB2, Quantity: 2}},
		{{ProductID: pB2, Quantity: 3}, {ProductID: pA2, Quantity: 2}},
		{{ProductID: pA2, Quantity: 1}, {ProductID: pC2, Quantity: 1}},
	} {
		if _, err := uc2.Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f2.gymID, ActorUserID: f2.ownerID, Method: "cash", Items: items,
		}); err != nil {
			t.Fatalf("order2 sale: %v", err)
		}
	}
	var bA, bB, bC, totalB int64
	_ = f2.db.Get(&bA, "SELECT stock FROM products WHERE id=?", pA2.String())
	_ = f2.db.Get(&bB, "SELECT stock FROM products WHERE id=?", pB2.String())
	_ = f2.db.Get(&bC, "SELECT stock FROM products WHERE id=?", pC2.String())
	_ = f2.db.Get(&totalB, "SELECT COALESCE(SUM(amount),0) FROM payments WHERE concept='product'")

	if aA != bA || aB != bB || aC != bC {
		t.Errorf("stocks diverge: order1=(%d,%d,%d) order2=(%d,%d,%d)", aA, aB, aC, bA, bB, bC)
	}
	if totalA != totalB {
		t.Errorf("totals diverge: order1=%d order2=%d", totalA, totalB)
	}
}

// ---------------------------------------------------------------------------
// UC-026 — RefundSale (MVP delegate to UC-022)
// ---------------------------------------------------------------------------

func TestUC026_RefundSale_DelegatesToRefundPayment(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Botella", 30, 5)
	saleOut, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("sale: %v", err)
	}
	refundUC := billingApp.NewRefundSale(
		f.saleRepo,
		billingApp.NewRefundPayment(f.paymentRepo, f.folios, f.memberSvc, f.uow, f.recorder),
		f.uow,
	)
	out, err := refundUC.Execute(context.Background(), billingApp.RefundSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, SaleID: saleOut.SaleID,
		Reason: "Cliente devolvió", Method: "cash",
	})
	if err != nil {
		t.Fatalf("refund sale: %v", err)
	}
	if out.Amount >= 0 {
		t.Errorf("refund amount must be negative, got %v", out.Amount)
	}
	if out.Restored {
		t.Errorf("MVP path should not auto-restore stock")
	}
}

// ---------------------------------------------------------------------------
// UC-027 — CashClose
// ---------------------------------------------------------------------------

func TestUC027_Report_AggregatesByMethodAndConcept(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Agua", 20, 100)

	// Membership cash payment from Sesión 3 fixture (registerPayment).
	if _, err := f.registerPayment().Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
	}); err != nil {
		t.Fatalf("membership cobro: %v", err)
	}
	// Two product sales — one cash, one transfer.
	for _, m := range []string{"cash", "transfer"} {
		if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, Method: m,
			Items: []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}},
		}); err != nil {
			t.Fatalf("sale %s: %v", m, err)
		}
	}

	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	rep, err := uc.Report(context.Background(), reportsApp.CashCloseReportInput{
		GymID: f.gymID, Date: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.Totals.GrandTotal <= 0 {
		t.Errorf("grand total should be > 0, got %v", rep.Totals.GrandTotal)
	}
	// cash = membership(600) + product(20) = 620.
	if got := rep.Totals.ByMethod["cash"]; got != 620 {
		t.Errorf("cash by method = %v, want 620", got)
	}
	// transfer = 20 product.
	if got := rep.Totals.ByMethod["transfer"]; got != 20 {
		t.Errorf("transfer = %v, want 20", got)
	}
	// concepts: membership 600, product 40.
	if rep.Totals.ByConcept["membership"] != 600 || rep.Totals.ByConcept["product"] != 40 {
		t.Errorf("by concept = %v", rep.Totals.ByConcept)
	}
}

func TestUC027_Close_PersistsCashCloseEvent(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Agua", 20, 5)
	if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}},
	}); err != nil {
		t.Fatalf("sale: %v", err)
	}
	counted := 18.0
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	out, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: time.Now().UTC(),
		CountedCash: &counted,
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if out.CalculatedCash != 20 {
		t.Errorf("calculated = %v, want 20", out.CalculatedCash)
	}
	if out.Discrepancy == nil || *out.Discrepancy != -2 {
		t.Errorf("discrepancy = %v, want -2", out.Discrepancy)
	}
	var n int
	_ = f.db.Get(&n, "SELECT COUNT(*) FROM cash_close_events WHERE id=?", out.CashCloseID.String())
	if n != 1 {
		t.Errorf("cash_close_events row missing")
	}
}

func TestUC027_Close_OptionalCountedCash(t *testing.T) {
	f := setupSales(t)
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	out, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: time.Now().UTC(),
		CountedCash: nil,
	})
	if err != nil {
		t.Fatalf("close skip: %v", err)
	}
	if out.Discrepancy != nil {
		t.Errorf("no count → no discrepancy expected, got %v", *out.Discrepancy)
	}
}
