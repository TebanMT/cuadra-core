//go:build sidecar

package app_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
)

// Ganancia realizada de productos del período (revenue − COGS), excluyendo
// ventas reembolsadas. El COGS aplica el costo unitario promedio ACTUAL del
// producto a ventas pasadas (aproximación deliberada de Standard).

func rpFloatEq(a, b float64) bool { return math.Abs(a-b) < 0.005 }

func monthRange() (time.Time, time.Time) {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	return monthStart, today
}

// seedProductWithCost crea un producto con stock inicial y costo unitario
// capturado (entrada restock con cost).
func (f *salesFixture) seedProductWithCost(t *testing.T, name string, price float64, stock int, unitCost float64) uuid.UUID {
	t.Helper()
	c := unitCost
	uc := prodApp.NewCreateProduct(f.productRepo, f.stockMovementRepo, f.uow, f.recorder)
	out, err := uc.Execute(context.Background(), prodApp.CreateProductInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		Name: name, Price: price, InitialStock: stock, InitialCost: &c,
	})
	if err != nil {
		t.Fatalf("seed product %s: %v", name, err)
	}
	return out.ProductID
}

func (f *salesFixture) realizedBetween(t *testing.T, from, to time.Time) (revenue, cogs float64, itemsTotal, itemsWithCost int) {
	t.Helper()
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	r, err := reportsInfra.NewSQLiteReader().RealizedProductProfitBetween(tx, f.gymID, from, to)
	if err != nil {
		t.Fatalf("realized: %v", err)
	}
	return r.Revenue, r.COGS, r.ItemsTotal, r.ItemsWithCost
}

func (f *salesFixture) refundSale(t *testing.T, saleID uuid.UUID) {
	t.Helper()
	refundUC := billingApp.NewRefundSale(
		f.saleRepo,
		billingApp.NewRefundPayment(f.paymentRepo, f.folios, f.memberSvc, f.uow, f.recorder),
		f.uow,
	)
	if _, err := refundUC.Execute(context.Background(), billingApp.RefundSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, SaleID: saleID, Method: "cash", Reason: "test",
	}); err != nil {
		t.Fatalf("refund: %v", err)
	}
}

// TestRealizedProfit_RevenueCOGSCoverage — revenue cuenta todas las líneas;
// COGS solo las de productos con costo; la cobertura reporta la diferencia.
func TestRealizedProfit_RevenueCOGSCoverage(t *testing.T) {
	f := setupSales(t)
	agua := f.seedProductWithCost(t, "Agua", 20, 100, 8.00) // costo unit 8
	sinCosto := f.seedProduct(t, "SinCosto", 25, 100)       // sin costo

	if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{
			{ProductID: agua, Quantity: 2},
			{ProductID: sinCosto, Quantity: 1},
		},
	}); err != nil {
		t.Fatalf("sale: %v", err)
	}

	from, to := monthRange()
	rev, cogs, items, withCost := f.realizedBetween(t, from, to)
	// revenue = 2·20 + 1·25 = 65; cogs = 2·8 = 16 (SinCosto no aporta);
	// realized = 49; cobertura 1 de 2 líneas con costo.
	if !rpFloatEq(rev, 65) {
		t.Errorf("revenue = %v, want 65", rev)
	}
	if !rpFloatEq(cogs, 16) {
		t.Errorf("cogs = %v, want 16", cogs)
	}
	if !rpFloatEq(rev-cogs, 49) {
		t.Errorf("realized = %v, want 49", rev-cogs)
	}
	if items != 2 || withCost != 1 {
		t.Errorf("cobertura = %d/%d, want 1/2", withCost, items)
	}
}

// TestRealizedProfit_ExcludesRefundedSales — una venta reembolsada no aporta
// ni a revenue ni a COGS ni a la cobertura.
func TestRealizedProfit_ExcludesRefundedSales(t *testing.T) {
	f := setupSales(t)
	agua := f.seedProductWithCost(t, "Agua", 20, 100, 8.00)

	// Venta 1 (se conserva): Agua x2.
	if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: agua, Quantity: 2}},
	}); err != nil {
		t.Fatalf("sale1: %v", err)
	}
	// Venta 2 (se reembolsa): Agua x1.
	sale2, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: agua, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("sale2: %v", err)
	}
	f.refundSale(t, sale2.SaleID)

	from, to := monthRange()
	rev, cogs, items, withCost := f.realizedBetween(t, from, to)
	// Solo venta 1: rev=40, cogs=16, realized=24, 1 línea con costo.
	if !rpFloatEq(rev, 40) || !rpFloatEq(cogs, 16) || !rpFloatEq(rev-cogs, 24) {
		t.Errorf("rev=%v cogs=%v realized=%v, want 40/16/24", rev, cogs, rev-cogs)
	}
	if items != 1 || withCost != 1 {
		t.Errorf("cobertura = %d/%d, want 1/1 (venta reembolsada excluida)", withCost, items)
	}
}

// TestRealizedProfit_FiltersByPaymentDate — una venta del mes en curso entra
// en la ventana del mes pero NO en la ventana del mes anterior.
func TestRealizedProfit_FiltersByPaymentDate(t *testing.T) {
	f := setupSales(t)
	agua := f.seedProductWithCost(t, "Agua", 20, 100, 8.00)
	if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: agua, Quantity: 1}},
	}); err != nil {
		t.Fatalf("sale: %v", err)
	}

	monthStart, today := monthRange()
	// Ventana del mes: incluye la venta (rev 20, cogs 8).
	rev, cogs, items, _ := f.realizedBetween(t, monthStart, today)
	if !rpFloatEq(rev, 20) || !rpFloatEq(cogs, 8) || items != 1 {
		t.Errorf("mes actual: rev=%v cogs=%v items=%d, want 20/8/1", rev, cogs, items)
	}
	// Ventana del mes anterior: sin ventas → todo en cero.
	prevStart := monthStart.AddDate(0, -1, 0)
	prevEnd := monthStart.AddDate(0, 0, -1)
	pRev, pCogs, pItems, _ := f.realizedBetween(t, prevStart, prevEnd)
	if !rpFloatEq(pRev, 0) || !rpFloatEq(pCogs, 0) || pItems != 0 {
		t.Errorf("mes anterior: rev=%v cogs=%v items=%d, want 0/0/0", pRev, pCogs, pItems)
	}
}
