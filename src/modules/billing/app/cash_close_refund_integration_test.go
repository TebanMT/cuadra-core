//go:build sidecar

package app_test

import (
	"context"
	"math"
	"testing"
	"time"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
)

// Corte de caja con un reembolso en EFECTIVO: verifica que (a) el neto del
// día RESTE el reembolso (no lo sume) y (b) el efectivo calculado del cajón
// descuente el reembolso cash (que físicamente salió). Antes el neto se
// inflaba al doble del reembolso y el cajón marcaba un faltante fantasma.
func TestCashClose_CashRefund_NetAndDrawer(t *testing.T) {
	f := setupSales(t)
	agua := f.seedProduct(t, "Agua", 20, 100)

	// Venta 1 (se conserva): Agua x5 = $100 cash.
	if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: agua, Quantity: 5}},
	}); err != nil {
		t.Fatalf("sale1: %v", err)
	}
	// Venta 2: Agua x2 = $40 cash, luego reembolsada en efectivo.
	sale2, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: agua, Quantity: 2}},
	})
	if err != nil {
		t.Fatalf("sale2: %v", err)
	}
	f.refundSale(t, sale2.SaleID)

	cashClose := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	now := time.Now().UTC()

	// (a) Report: NetTotal = ingresos($140) − |reembolso|($40) − gastos(0) = $100.
	rep, err := cashClose.Report(context.Background(), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: now})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !ccEq(rep.NetTotal, 100) {
		t.Errorf("NetTotal = %v, want 100 (140 − 40 reembolso)", rep.NetTotal)
	}
	// ByMethod[cash] = $140 (ambos cobros product); RefundByMethod[cash] = −$40.
	if !ccEq(rep.Totals.ByMethod["cash"], 140) {
		t.Errorf("ByMethod[cash] = %v, want 140", rep.Totals.ByMethod["cash"])
	}
	if !ccEq(rep.Totals.RefundByMethod["cash"], -40) {
		t.Errorf("RefundByMethod[cash] = %v, want -40", rep.Totals.RefundByMethod["cash"])
	}

	// (b) Close: el efectivo calculado del cajón = 140 − 40 reembolso = $100.
	out, err := cashClose.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: now,
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if !ccEq(out.CalculatedCash, 100) {
		t.Errorf("CalculatedCash = %v, want 100 (entró 140, salió 40 por reembolso)", out.CalculatedCash)
	}
}

func ccEq(a, b float64) bool { return math.Abs(a-b) < 0.005 }
