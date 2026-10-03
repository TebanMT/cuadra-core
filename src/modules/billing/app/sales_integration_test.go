//go:build sidecar

package app_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
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
	).WithGyms(f.gymRepo)
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

func TestUC025_SaleBeyondRecordedStockKeepsPaymentAndNegativeBalance(t *testing.T) {
	f := setupSales(t)
	plenty := f.seedProduct(t, "Plenty", 10, 50)
	scarce := f.seedProduct(t, "Scarce", 30, 1)
	out, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", Items: []billingApp.SaleLineInput{{ProductID: plenty, Quantity: 5}, {ProductID: scarce, Quantity: 5}}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 200 || out.Paid != 200 {
		t.Fatalf("wrong collection: %+v", out)
	}
	for id, want := range map[uuid.UUID]int{plenty: 45, scarce: -4} {
		var stock int
		if e := f.db.Get(&stock, "SELECT stock FROM products WHERE id=?", id.String()); e != nil || stock != want {
			t.Fatalf("stock=%d want=%d err=%v", stock, want, e)
		}
	}
	for query, want := range map[string]int{"SELECT COUNT(*) FROM payments WHERE concept='product'": 1, "SELECT COUNT(*) FROM sales": 1, "SELECT COUNT(*) FROM sale_items": 2, "SELECT COUNT(*) FROM stock_movements WHERE movement_type='sale'": 2} {
		var count int
		if e := f.db.Get(&count, query); e != nil || count != want {
			t.Fatalf("%s: %d want %d (%v)", query, count, want, e)
		}
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

// ---------------------------------------------------------------------------
// UC-025 — Fiado (venta a crédito con saldo pendiente)
// ---------------------------------------------------------------------------
// Cuando `Paid < Total`, RegisterSale crea el Payment con balance_pending
// > 0 y exige socio asociado. El saldo se liquida después por el flujo
// estándar UC-019 (SettlePendingBalance) — mismo path que las mensualidades
// pagadas en partes.

func TestUC025_Fiado_CreatesBalancePending(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Proteína", 600, 3)
	mid := f.memberID
	paid := 200.0 // total = 600, queda 400 a deber
	uc := f.registerSale()
	out, err := uc.Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		MemberID: &mid,
		Paid:     &paid,
		Items:    []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("fiado sale: %v", err)
	}
	if out.Total != 600 || out.Paid != 200 || out.BalancePending != 400 {
		t.Errorf("got total=%v paid=%v balance=%v (want 600/200/400)",
			out.Total, out.Paid, out.BalancePending)
	}
	var amount, balance int64
	_ = f.db.Get(&amount, "SELECT amount FROM payments WHERE id=?", out.PaymentID.String())
	_ = f.db.Get(&balance, "SELECT balance_pending FROM payments WHERE id=?", out.PaymentID.String())
	if amount != 20000 || balance != 40000 {
		t.Errorf("persisted cents: amount=%d balance=%d (want 20000/40000)", amount, balance)
	}
}

func TestUC025_Fiado_RequiresMember(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Snickers", 25, 10)
	paid := 10.0 // intento de fiado sin socio asociado
	uc := f.registerSale()
	_, err := uc.Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Paid:  &paid,
		Items: []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}},
	})
	if err == nil {
		t.Errorf("fiado sin socio debería fallar")
	}
}

func TestUC025_Fiado_FullSettlementClearsBalance(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Gatorade", 25, 20)
	mid := f.memberID
	paid := 10.0 // total 25, queda 15 a deber
	saleOut, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		MemberID: &mid, Paid: &paid,
		Items: []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("fiado: %v", err)
	}

	// Liquidación posterior usando el flujo estándar — debería bajar
	// balance_pending a 0 en el Payment original.
	settle := billingApp.NewSettlePendingBalance(f.paymentRepo, f.folios, f.uow, f.recorder)
	if _, err := settle.Execute(context.Background(), billingApp.SettlePendingBalanceInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ParentPaymentID: saleOut.PaymentID,
		Amount:          15, Method: "cash",
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	var bal int64
	_ = f.db.Get(&bal, "SELECT balance_pending FROM payments WHERE id=?", saleOut.PaymentID.String())
	if bal != 0 {
		t.Errorf("balance after full settle = %d cents, want 0", bal)
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
// UC-026 — RefundSale
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
	refundUC := financedRefundUC(f)
	out, err := refundUC.Execute(context.Background(), billingApp.RefundSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, SaleID: saleOut.SaleID,
		Reason: "Cliente devolvió", Method: "cash", IdempotencyKey: "refund-botella",
		Items: []refundDomain.ItemInput{{SaleItemID: saleOut.Items[0].SaleItemID, Quantity: 1, Disposition: refundDomain.Damaged}},
	})
	if err != nil {
		t.Fatalf("refund sale: %v", err)
	}
	if out.Amount != 30 {
		t.Errorf("refund amount must be positive magnitude 30, got %v", out.Amount)
	}
	if out.Restored {
		t.Errorf("damaged units must not be reported as restored")
	}
}

func TestUC026_RefundSale_ReportsAndAppliesExplicitStockReturn(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 15, 5)
	saleOut, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 2}},
	})
	if err != nil {
		t.Fatalf("sale: %v", err)
	}
	out, err := financedRefundUC(f).Execute(context.Background(), billingApp.RefundSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, SaleID: saleOut.SaleID,
		Reason: "Una unidad regresó en buen estado", Method: "cash", IdempotencyKey: "refund-agua-stock",
		Items: []refundDomain.ItemInput{{SaleItemID: saleOut.Items[0].SaleItemID, Quantity: 1, Disposition: refundDomain.ReturnedToStock}},
	})
	if err != nil {
		t.Fatalf("refund sale: %v", err)
	}
	if !out.Restored {
		t.Fatal("returned_to_stock must be reported as restored")
	}
	var stock int
	if err := f.db.Get(&stock, "SELECT stock FROM products WHERE id=?", productID.String()); err != nil {
		t.Fatalf("read stock: %v", err)
	}
	if stock != 4 { // initial 5 − sold 2 + returned 1
		t.Fatalf("stock=%d, want 4", stock)
	}
}

// ---------------------------------------------------------------------------
// UC-027 — CashClose
// ---------------------------------------------------------------------------

func TestUC027_Report_AggregatesByMethodAndConcept(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Agua", 20, 100)
	day := cashSessionBusinessDay(t)

	// Membership cash payment from Sesión 3 fixture (registerPayment).
	if _, err := f.registerPayment().Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash", PaymentDate: day,
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
		GymID: f.gymID, Date: day,
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
	// Digital methods do not belong to a drawer, but remain visible as a
	// gym-wide reference. calculateDrawerCash must still use cash only.
	if got := rep.Totals.ByMethod["transfer"]; got != 20 {
		t.Errorf("transfer = %v, want 20 as gym-wide reference", got)
	}
	if rep.Totals.GrandTotal != 640 {
		t.Errorf("grand total = %v, want 640 including digital reference", rep.Totals.GrandTotal)
	}
	// concepts: membership 600, cash product 20 and transfer product 20.
	if rep.Totals.ByConcept["membership"].Total != 600 || rep.Totals.ByConcept["product"].Total != 40 {
		t.Errorf("by concept = %v", rep.Totals.ByConcept)
	}
	if rep.UncoveredCashActivity != 620 {
		t.Errorf("physical cash activity = %v, want 620; digital must not affect it", rep.UncoveredCashActivity)
	}
}

func TestUC027_Close_PersistsCashCloseEvent(t *testing.T) {
	f := setupSales(t)
	day := cashSessionBusinessDay(t)
	p := f.seedProduct(t, "Agua", 20, 5)
	if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}},
	}); err != nil {
		t.Fatalf("sale: %v", err)
	}
	counted := 18.0
	differenceReason := "faltaron dos pesos en el conteo"
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	out, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day,
		CountedCash: &counted, DiscrepancyReason: &differenceReason,
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

func TestUC027_Close_IsImmutablePerGymAndDay(t *testing.T) {
	f := setupSales(t)
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	date := cashSessionBusinessDay(t)
	if _, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: date,
	}); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if _, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: date,
	}); err == nil {
		t.Fatal("second close for same gym/day must be rejected")
	}
	var n int
	if err := f.db.Get(&n, `SELECT COUNT(*) FROM cash_close_events WHERE gym_id=? AND close_date=? AND deleted_at IS NULL`,
		f.gymID.String(), date.Format("2006-01-02")); err != nil {
		t.Fatalf("count closes: %v", err)
	}
	if n != 1 {
		t.Errorf("live closes = %d, want 1", n)
	}
}

func TestUC027_Reopen_PreservesAuditAndAllowsCorrectedClose(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	registerSale := func(method string) {
		t.Helper()
		if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, Method: method,
			Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
		}); err != nil {
			t.Fatalf("%s sale: %v", method, err)
		}
	}

	registerSale("cash")
	date := cashSessionBusinessDay(t)
	countedFirst := 20.0
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	first, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: date, CountedCash: &countedFirst,
	})
	if err != nil {
		t.Fatalf("first close: %v", err)
	}

	// A transfer after close changes income, but not the physical drawer.
	registerSale("transfer")
	report, err := uc.Report(context.Background(), reportsApp.CashCloseReportInput{
		GymID: f.gymID, Date: date,
	})
	if err != nil {
		t.Fatalf("report after transfer: %v", err)
	}
	if report.Closed == nil || report.Closed.IsOutdated {
		t.Fatalf("transfer must not stale a cash close: %+v", report.Closed)
	}

	// An extra cash income arrives while the money from the first close still
	// sits in the drawer. The report must flag the stored snapshot as stale.
	registerSale("cash")
	report, err = uc.Report(context.Background(), reportsApp.CashCloseReportInput{
		GymID: f.gymID, Date: date,
	})
	if err != nil {
		t.Fatalf("report after late income: %v", err)
	}
	if report.Closed == nil || !report.Closed.IsOutdated {
		t.Fatalf("closed report = %+v, want outdated", report.Closed)
	}
	if report.Closed.CalculatedCash != 20 || report.Closed.CurrentCalculatedCash != 40 {
		t.Errorf("cash snapshot/current = %.2f/%.2f, want 20/40", report.Closed.CalculatedCash, report.Closed.CurrentCalculatedCash)
	}

	reopenReason := "reapertura manual"
	if err := uc.Reopen(context.Background(), reportsApp.CashReopenInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: date, Reason: &reopenReason,
	}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var reopened struct {
		Status    string        `db:"status"`
		DeletedAt sql.NullInt64 `db:"deleted_at"`
	}
	if err := f.db.Get(&reopened, `SELECT status,deleted_at FROM cash_close_events WHERE id=?`, first.CashCloseID.String()); err != nil {
		t.Fatalf("read reopened session: %v", err)
	}
	if reopened.DeletedAt.Valid || reopened.Status != "stale" {
		t.Fatalf("reopened session = %+v, want same live stale row", reopened)
	}
	var reopenAudit int
	if err := f.db.Get(&reopenAudit, `SELECT COUNT(*) FROM audit_log WHERE gym_id=? AND entity_type='cash_close_events' AND entity_id=? AND action='update' AND changes LIKE '%reapertura manual%'`,
		f.gymID.String(), first.CashCloseID.String()); err != nil {
		t.Fatalf("read reopen audit: %v", err)
	}
	if reopenAudit != 1 {
		t.Fatalf("reopen audit rows = %d, want 1", reopenAudit)
	}

	countedCorrected := 40.0
	corrected, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: date, CountedCash: &countedCorrected,
	})
	if err != nil {
		t.Fatalf("corrected close: %v", err)
	}
	if corrected.CashCloseID != first.CashCloseID || corrected.CalculatedCash != 40 {
		t.Errorf("corrected close = %+v, want same natural session refreshed to $40", corrected)
	}
	var live, historical int
	if err := f.db.Get(&live, `SELECT COUNT(*) FROM cash_close_events WHERE gym_id=? AND close_date=? AND deleted_at IS NULL`,
		f.gymID.String(), date.Format("2006-01-02")); err != nil {
		t.Fatalf("count live closes: %v", err)
	}
	if err := f.db.Get(&historical, `SELECT COUNT(*) FROM cash_close_events WHERE gym_id=? AND close_date=?`,
		f.gymID.String(), date.Format("2006-01-02")); err != nil {
		t.Fatalf("count all closes: %v", err)
	}
	if live != 1 || historical != 1 {
		t.Errorf("closes live/history = %d/%d, want one stable natural session", live, historical)
	}
}

func TestUC027_Close_OptionalCountedCash(t *testing.T) {
	f := setupSales(t)
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	out, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: cashSessionBusinessDay(t),
		CountedCash: nil,
	})
	if err != nil {
		t.Fatalf("close skip: %v", err)
	}
	if out.Discrepancy != nil {
		t.Errorf("no count → no discrepancy expected, got %v", *out.Discrepancy)
	}
}

func TestUC027_Close_RejectsFutureGymDayButAllowsBackdatingAndSessionIDReopen(t *testing.T) {
	f := setupSales(t)
	today := cashSessionBusinessDay(t)
	future := today.AddDate(0, 0, 1)
	backdated := today.AddDate(0, 0, -1)
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithGyms(f.gymRepo)

	if _, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: future,
	}); !errors.Is(err, billingErrors.ErrCashCloseFutureDate) {
		t.Fatalf("future close error = %v, want %v", err, billingErrors.ErrCashCloseFutureDate)
	}
	var futureRows int
	if err := f.db.Get(&futureRows,
		`SELECT COUNT(*) FROM cash_close_events WHERE gym_id=? AND close_date=?`,
		f.gymID.String(), future.Format("2006-01-02")); err != nil || futureRows != 0 {
		t.Fatalf("future close must not persist: rows=%d err=%v", futureRows, err)
	}

	closed, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: backdated,
	})
	if err != nil {
		t.Fatalf("backdated close: %v", err)
	}
	reason := "corregir corte atrasado"
	if err := uc.Reopen(context.Background(), reportsApp.CashReopenInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: future, Reason: &reason,
	}); !errors.Is(err, billingErrors.ErrCashCloseFutureDate) {
		t.Fatalf("future date alias reopen error = %v, want %v", err, billingErrors.ErrCashCloseFutureDate)
	}
	// Once a concrete historical session is addressed by ID, Date is not a
	// selector and must not make that legitimate correction fail.
	if err := uc.Reopen(context.Background(), reportsApp.CashReopenInput{
		GymID: f.gymID, ActorUserID: f.ownerID, SessionID: closed.CashCloseID,
		Date: future, Reason: &reason,
	}); err != nil {
		t.Fatalf("session-ID reopen must ignore alias date: %v", err)
	}
}

// Pin del contrato total_pending (UC-021): el FE tipaba
// PaymentHistoryResponse.total_pending desde siempre, pero ningún handler
// lo emitía — el banner de deuda del perfil y el chip del POS nunca
// prendían. El rollup debe sumar TODA la deuda del socio (cualquier
// concepto) ignorando filtros y paginación: sumar sólo la página visible
// escondería deudas viejas.
func TestUC021_ListMemberPayments_TotalPendingRollup(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Agua", 63, 10)
	mid := f.memberID
	paid := 30.0 // total 63 → deben quedar 33 a deber
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		MemberID: &mid, Paid: &paid,
		Items: []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("fiado: %v", err)
	}

	list := billingApp.NewListMemberPayments(f.paymentRepo, f.memberRepo, f.uow).WithSales(f.saleItemRepo)

	out, err := list.Execute(context.Background(), billingApp.ListMemberPaymentsInput{
		GymID: f.gymID, MemberID: mid, Page: 1, PageSize: 25,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out.TotalPending != 33 {
		t.Errorf("total_pending = %v, want 33", out.TotalPending)
	}
	if len(out.Items) != 1 || out.SaleIDs[out.Items[0].ID] != sale.SaleID {
		t.Fatalf("sale ids=%+v, want payment %s -> sale %s", out.SaleIDs, sale.PaymentID, sale.SaleID)
	}

	// El rollup ignora el filtro por concepto: aunque la página venga
	// vacía (filtro membership sobre una deuda de venta), la deuda total
	// del socio se sigue reportando.
	filtered, err := list.Execute(context.Background(), billingApp.ListMemberPaymentsInput{
		GymID: f.gymID, MemberID: mid, ConceptFilter: "membership", Page: 1, PageSize: 25,
	})
	if err != nil {
		t.Fatalf("list filtrado: %v", err)
	}
	if len(filtered.Items) != 0 {
		t.Errorf("items con filtro membership = %d, want 0 (la deuda es de venta)", len(filtered.Items))
	}
	if filtered.TotalPending != 33 {
		t.Errorf("total_pending con filtro = %v, want 33 (el rollup no respeta filtros)", filtered.TotalPending)
	}
}
