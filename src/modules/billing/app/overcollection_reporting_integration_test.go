//go:build sidecar

package app_test

import (
	"context"
	"testing"

	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
)

// Pins acceptance scenarios 11 and 14 end to end: correcting a real cash
// collection rewrites recognized income on the original economic day, while
// settling the resulting overcollection later moves physical cash only. The
// later settlement must never become a second financial outflow.
func TestCorrectSale_PendingOvercollectionReportsOriginalIncomeAndLaterCashOnly(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 1, 50)
	day1, day2 := gymTestDay(-2), gymTestDay(-1)

	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", PaymentDate: day1,
		IdempotencyKey: "sale-overcollection-reporting",
		Items:          []billingApp.SaleLineInput{{ProductID: productID, Quantity: 40}},
	})
	if err != nil {
		t.Fatal(err)
	}
	corrections := financedCorrectionUC(f)
	detail, err := corrections.Detail(context.Background(), f.gymID, sale.SaleID)
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := corrections.Execute(context.Background(), billingApp.CorrectSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
		ExpectedVersion: 0, Reason: "Se registraron 40 aguas; eran 4",
		MoneyResolution: "refund_pending", IdempotencyKey: "correct-overcollection-reporting",
		Lines: []billingApp.CorrectSaleLineInput{{
			SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 4,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if corrected.MoneyEffect.RecognizedIncome != 4 || corrected.MoneyEffect.PendingRefundDue != 36 || corrected.MoneyEffect.RefundedNow != 0 {
		t.Fatalf("correction money effect = %+v, want income=4 pending=36 refunded=0", corrected.MoneyEffect)
	}

	settled, err := corrections.SettlePending(context.Background(), billingApp.SettlePendingRefundInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", CorrectionID: corrected.CorrectionID,
		Method: "cash", CashDrawerID: &f.gymID, PaymentDate: day2,
		IdempotencyKey: "settle-overcollection-reporting",
	})
	if err != nil {
		t.Fatal(err)
	}
	if settled.Amount != 36 || settled.PendingAmount != 0 {
		t.Fatalf("settlement = %+v, want amount=36 pending=0", settled)
	}
	after, err := corrections.Detail(context.Background(), f.gymID, sale.SaleID)
	if err != nil {
		t.Fatal(err)
	}
	if after.PendingRefundDue != 0 {
		t.Fatalf("pending refund after settlement = %.2f, want 0", after.PendingRefundDue)
	}

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := reportsInfra.NewSQLiteReader()
	day1Financial, err := reader.CanonicalFinancialBetween(tx, f.gymID, "America/Mexico_City", day1, day1)
	if err != nil {
		t.Fatal(err)
	}
	day2Financial, err := reader.CanonicalFinancialBetween(tx, f.gymID, "America/Mexico_City", day2, day2)
	if err != nil {
		t.Fatal(err)
	}
	cumulative, err := reader.CanonicalFinancialBetween(tx, f.gymID, "America/Mexico_City", day1, day2)
	if err != nil {
		t.Fatal(err)
	}
	day2Cash, err := reader.SumCashClosedBetween(tx, f.gymID, day2, day2)
	if err != nil {
		t.Fatal(err)
	}

	day1Result := day1Financial.MembershipIncome + day1Financial.ProductIncome + day1Financial.OtherIncome + day1Financial.UnclassifiedIncome - day1Financial.OperatingExpenses - day1Financial.InventoryPurchases - day1Financial.Refunds
	day2Result := day2Financial.MembershipIncome + day2Financial.ProductIncome + day2Financial.OtherIncome + day2Financial.UnclassifiedIncome - day2Financial.OperatingExpenses - day2Financial.InventoryPurchases - day2Financial.Refunds
	cumulativeResult := cumulative.MembershipIncome + cumulative.ProductIncome + cumulative.OtherIncome + cumulative.UnclassifiedIncome - cumulative.OperatingExpenses - cumulative.InventoryPurchases - cumulative.Refunds
	if day1Financial.ProductIncome != 4 || day1Financial.Refunds != 0 || day1Result != 4 {
		t.Fatalf("day 1 financial = %+v result=%.2f, want product income=4 refunds=0 result=4", day1Financial, day1Result)
	}
	if day2Financial.ProductIncome != 0 || day2Financial.Refunds != 0 || day2Result != 0 || day2Cash != -36 {
		t.Fatalf("day 2 financial = %+v result=%.2f cash=%.2f, want result=0 cash=-36", day2Financial, day2Result, day2Cash)
	}
	if cumulative.ProductIncome != 4 || cumulative.Refunds != 0 || cumulativeResult != 4 {
		t.Fatalf("cumulative financial = %+v result=%.2f, want product income=4 refunds=0 result=4", cumulative, cumulativeResult)
	}
}
