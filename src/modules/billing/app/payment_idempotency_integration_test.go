//go:build sidecar

package app_test

import (
	"context"
	"reflect"
	"testing"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
)

func TestMembershipPayment_IdempotencyReplaysOriginalWithoutRenewingTwice(t *testing.T) {
	f := setup(t)
	in := billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		MembershipTypeID: f.planID, Method: "cash", IdempotencyKey: "membership-retry-1",
	}
	first, err := f.registerPayment().Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.registerPayment().Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay differs:\nfirst=%+v\nsecond=%+v", first, second)
	}
	var payments int
	_ = f.db.Get(&payments, `SELECT COUNT(*) FROM payments WHERE idempotency_key='membership-retry-1'`)
	if payments != 1 {
		t.Fatalf("payments=%d, want 1", payments)
	}
	conflict := in
	conflict.PaidNow = 10
	if _, err := f.registerPayment().Execute(context.Background(), conflict); err == nil {
		t.Fatal("same key with changed amount must conflict")
	}
}

func TestSale_IdempotencyReplaysOriginalWithoutDecrementingStockTwice(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	in := billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", IdempotencyKey: "sale-retry-1",
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 2}},
	}
	first, err := f.registerSale().Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.registerSale().Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay differs:\nfirst=%+v\nsecond=%+v", first, second)
	}
	var stock, sales int
	_ = f.db.Get(&stock, `SELECT stock FROM products WHERE id=?`, productID.String())
	_ = f.db.Get(&sales, `SELECT COUNT(*) FROM sales WHERE payment_id=?`, first.PaymentID.String())
	if stock != 8 || sales != 1 {
		t.Fatalf("stock=%d sales=%d, want 8/1", stock, sales)
	}
	conflict := in
	conflict.Items = []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}}
	if _, err := f.registerSale().Execute(context.Background(), conflict); err == nil {
		t.Fatal("same key with changed cart must conflict")
	}
}

func TestSettlement_IdempotencyReplaysOriginalBalance(t *testing.T) {
	f := setup(t)
	root, err := f.registerPayment().Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		MembershipTypeID: f.planID, Method: "cash", PaidNow: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := billingApp.SettlePendingBalanceInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ParentPaymentID: root.PaymentID,
		Amount: 100, Method: "transfer", IdempotencyKey: "settlement-retry-1",
	}
	uc := billingApp.NewSettlePendingBalance(f.paymentRepo, f.folios, f.uow, f.recorder)
	first, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay differs:\nfirst=%+v\nsecond=%+v", first, second)
	}
	var balance, settlements int
	_ = f.db.Get(&balance, `SELECT balance_pending FROM payments WHERE id=?`, root.PaymentID.String())
	_ = f.db.Get(&settlements, `SELECT COUNT(*) FROM payments WHERE parent_payment_id=? AND concept='balance_settlement'`, root.PaymentID.String())
	if balance != 20000 || settlements != 1 {
		t.Fatalf("balance=%d settlements=%d, want 20000/1", balance, settlements)
	}
	conflict := in
	conflict.Amount = 50
	if _, err := uc.Execute(context.Background(), conflict); err == nil {
		t.Fatal("same key with changed settlement amount must conflict")
	}
}
