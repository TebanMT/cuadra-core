//go:build sidecar

package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	controllers "github.com/cuadra/cuadra-core/src/modules/billing/interfaces/controllers"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
)

func TestFullCreditSale_StockDebtCashAndIdempotentSettlement(t *testing.T) {
	f := setupSales(t)
	product := f.seedProduct(t, "Agua fiada", 20, 1)
	zero, total := 0.0, 40.0
	day := cashSessionBusinessDay(t)
	input := billingApp.RegisterSaleInput{GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", MemberID: &f.memberID,
		Paid: &zero, ExpectedTotal: &total, PaymentDate: day, IdempotencyKey: "full-credit-sale", Items: []billingApp.SaleLineInput{{ProductID: product, Quantity: 2}}}
	sale, err := f.registerSale().Execute(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if sale.Paid != 0 || sale.BalancePending != 40 {
		t.Fatalf("sale=%+v", sale)
	}
	var stock, amount, recognized int
	if err := f.db.Get(&stock, `SELECT stock FROM products WHERE id=?`, product); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Get(&amount, `SELECT amount FROM payments WHERE id=?`, sale.PaymentID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Get(&recognized, `SELECT recognized_amount FROM payments WHERE id=?`, sale.PaymentID); err != nil {
		t.Fatal(err)
	}
	if stock != -1 || amount != 0 || recognized != 0 {
		t.Fatalf("stock=%d amount=%d recognized=%d", stock, amount, recognized)
	}
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	report, err := cash.Report(context.Background(), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.GrandTotal != 0 || report.UncoveredCashActivity != 0 || report.RequiresNewSession || len(report.Totals.ByOperator) != 0 {
		t.Fatalf("credit generated cash activity: %+v", report)
	}
	// A retry after the catalog changed must return the same completed sale.
	if _, err := f.db.Exec(`UPDATE products SET price=2500 WHERE id=?`, product); err != nil {
		t.Fatal(err)
	}
	replay, err := f.registerSale().Execute(context.Background(), input)
	if err != nil || replay == nil || replay.SaleID != sale.SaleID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	settle := billingApp.NewSettlePendingBalance(f.paymentRepo, f.folios, f.uow, f.recorder).WithGyms(f.gymRepo)
	payment := billingApp.SettlePendingBalanceInput{GymID: f.gymID, ActorUserID: f.ownerID, ParentPaymentID: sale.PaymentID, Amount: 15, Method: "cash", PaymentDate: day, IdempotencyKey: "credit-abono"}
	first, err := settle.Execute(context.Background(), payment)
	if err != nil {
		t.Fatal(err)
	}
	second, err := settle.Execute(context.Background(), payment)
	if err != nil || first.SettlementID != second.SettlementID || second.NewBalancePending != 25 {
		t.Fatalf("settle replay=%+v err=%v", second, err)
	}
	report, err = cash.Report(context.Background(), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil || report.Totals.GrandTotal != 15 || report.UncoveredCashActivity != 15 {
		t.Fatalf("cash must contain only abono: %+v err=%v", report, err)
	}
	payment.IdempotencyKey = "credit-liquidacion"
	payment.Amount = 25
	payment.Method = "transfer"
	last, err := settle.Execute(context.Background(), payment)
	if err != nil || last.NewBalancePending != 0 {
		t.Fatalf("liquidacion=%+v err=%v", last, err)
	}
	var collected int
	if err := f.db.Get(&collected, `SELECT SUM(amount) FROM payments WHERE id=? OR parent_payment_id=?`, sale.PaymentID, sale.PaymentID); err != nil {
		t.Fatal(err)
	}
	if collected != 4000 {
		t.Fatalf("collected=%d", collected)
	}
	// Reprinting the original credit sale still describes its actual sale total,
	// even after later payments have cleared the balance.
	receipt := &creditReceiptCapture{}
	_, err = billingApp.NewSendReceipt(f.paymentRepo, f.uow).WithResender(receipt).Execute(context.Background(), billingApp.SendReceiptInput{GymID: f.gymID, PaymentID: sale.PaymentID})
	if err != nil || receipt.event.Amount != 40 || receipt.event.PaymentID != sale.PaymentID {
		t.Fatalf("receipt=%+v err=%v", receipt.event, err)
	}
}

type creditReceiptCapture struct {
	event billingApp.PaymentCompletedEvent
}

func (r *creditReceiptCapture) ResendReceipt(_ context.Context, event billingApp.PaymentCompletedEvent) (billingApp.ReceiptResendOutcome, error) {
	r.event = event
	return billingApp.ReceiptResendOutcome{Status: "queued"}, nil
}

func TestFullCreditSale_RefundCancelsDebtWithoutCash(t *testing.T) {
	f := setupSales(t)
	product := f.seedProduct(t, "Fiado devuelto", 25, 3)
	zero := 0.0
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", MemberID: &f.memberID, Paid: &zero, Items: []billingApp.SaleLineInput{{ProductID: product, Quantity: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := financedRefundUC(f).Execute(context.Background(), billingApp.RefundSaleInput{GymID: f.gymID, ActorUserID: f.ownerID, SaleID: sale.SaleID, Reason: "Regresaron los dos productos", IdempotencyKey: "credit-return", Items: []refundDomain.ItemInput{{SaleItemID: sale.Items[0].SaleItemID, Quantity: 2, Disposition: refundDomain.ReturnedToStock}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Amount != 0 {
		t.Fatalf("refund moved money: %+v", result)
	}
	var stock, balance, refundPayments int
	_ = f.db.Get(&stock, `SELECT stock FROM products WHERE id=?`, product)
	_ = f.db.Get(&balance, `SELECT balance_pending FROM payments WHERE id=?`, sale.PaymentID)
	_ = f.db.Get(&refundPayments, `SELECT COUNT(*) FROM payments WHERE concept='refund'`)
	if stock != 3 || balance != 0 || refundPayments != 0 {
		t.Fatalf("stock=%d balance=%d refundPayments=%d", stock, balance, refundPayments)
	}
}

func TestFullCreditSale_CorrectionPreservesZeroCollection(t *testing.T) {
	f := setupSales(t)
	product := f.seedProduct(t, "Fiado corregido", 25, 3)
	zero := 0.0
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", MemberID: &f.memberID, Paid: &zero, Items: []billingApp.SaleLineInput{{ProductID: product, Quantity: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = financedCorrectionUC(f).Execute(context.Background(), billingApp.CorrectSaleInput{GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID, ExpectedVersion: 0, Reason: "Se capturó una unidad de más", MoneyResolution: "record_only", IdempotencyKey: "credit-correction", Lines: []billingApp.CorrectSaleLineInput{{SaleItemID: &sale.Items[0].SaleItemID, ProductID: product, Quantity: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	var amount, balance, stock int
	_ = f.db.Get(&amount, `SELECT amount FROM payments WHERE id=?`, sale.PaymentID)
	_ = f.db.Get(&balance, `SELECT balance_pending FROM payments WHERE id=?`, sale.PaymentID)
	_ = f.db.Get(&stock, `SELECT stock FROM products WHERE id=?`, product)
	if amount != 0 || balance != 2500 || stock != 2 {
		t.Fatalf("amount=%d balance=%d stock=%d", amount, balance, stock)
	}
}

func TestSalePriceConfirmationAndInvalidDiscountRollback(t *testing.T) {
	f := setupSales(t)
	product := f.seedProduct(t, "Precio confirmado", 20, 3)
	expected := 10.0
	input := billingApp.RegisterSaleInput{GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", ExpectedTotal: &expected, Items: []billingApp.SaleLineInput{{ProductID: product, Quantity: 1}}}
	if _, err := f.registerSale().Execute(context.Background(), input); !errors.Is(err, billingErrors.ErrSaleTotalChanged) {
		t.Fatalf("price error=%v", err)
	}
	var stock int
	_ = f.db.Get(&stock, `SELECT stock FROM products WHERE id=?`, product)
	if stock != 3 {
		t.Fatalf("failed sale changed stock=%d", stock)
	}
	expected = 0
	input.Discount = 20
	input.IdempotencyKey = "free-sale"
	out, err := f.registerSale().Execute(context.Background(), input)
	if !errors.Is(err, billingErrors.ErrDiscountTooLarge) || out != nil {
		t.Fatalf("fully discounted=%+v err=%v", out, err)
	}
	_ = f.db.Get(&stock, `SELECT stock FROM products WHERE id=?`, product)
	if stock != 3 {
		t.Fatalf("invalid discount changed stock=%d", stock)
	}
}

func TestFullCreditSale_HTTPRecordsAndSettlesWithoutInitialPayment(t *testing.T) {
	f := setupSales(t)
	product := f.seedProduct(t, "Venta desde recepción", 20, 5)
	tokens := auth.NewJWTService("credit-sale-http-test-secret-with-enough-entropy")
	token, err := tokens.GenerateAccessToken(f.ownerID, f.gymID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	ctrl := &controllers.PaymentController{Tokens: tokens, RegisterSale: f.registerSale(), Settle: billingApp.NewSettlePendingBalance(f.paymentRepo, f.folios, f.uow, f.recorder).WithGyms(f.gymRepo)}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	ctrl.RegisterRoutes(router)
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	body := fmt.Sprintf(`{"payment_method":"cash","member_id":%q,"paid":0,"expected_total":20,"idempotency_key":"http-full-credit","line_items":[{"product_id":%q,"quantity":1}]}`, f.memberID.String(), product.String())
	result := post("/api/v1/sales", body)
	if result.Code != 201 {
		t.Fatalf("create=%d %s", result.Code, result.Body.String())
	}
	var sale struct {
		PaymentID string  `json:"payment_id"`
		Paid      float64 `json:"paid"`
		Balance   float64 `json:"balance_pending"`
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(envelope.Data, &sale); err != nil {
		t.Fatal(err)
	}
	if sale.Paid != 0 || sale.Balance != 20 || sale.PaymentID == "" {
		t.Fatalf("response=%s", result.Body.String())
	}
	replay := post("/api/v1/sales", body)
	if replay.Code != 201 || replay.Body.String() != result.Body.String() {
		t.Fatalf("replay=%d %s", replay.Code, replay.Body.String())
	}
	settled := post("/api/v1/payments/"+sale.PaymentID+"/settle", `{"amount":20,"payment_method":"cash","idempotency_key":"http-credit-settle"}`)
	if settled.Code != 201 {
		t.Fatalf("settle=%d %s", settled.Code, settled.Body.String())
	}
	var cash int
	_ = f.db.Get(&cash, `SELECT SUM(amount) FROM payments`)
	if cash != 2000 {
		t.Fatalf("cash=%d", cash)
	}
}
