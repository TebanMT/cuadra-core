package app

import (
	"testing"
	"time"

	"github.com/google/uuid"

	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
)

func TestRefundCommandFingerprint_CoversEverySemanticChoice(t *testing.T) {
	drawer, saleID, correctionID := uuid.New(), uuid.New(), uuid.New()
	lineA, lineB := uuid.New(), uuid.New()
	base := RefundPaymentInput{
		GymID: uuid.New(), ActorUserID: uuid.New(), ParentPaymentID: uuid.New(),
		Reason: "Producto equivocado", Method: "cash", CashDrawerID: &drawer,
		Amount: 25, PaymentDate: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC),
		SaleID: &saleID, CorrectionID: &correctionID, Kind: refundDomain.KindRevenueRefund,
		Items: []refundDomain.ItemInput{
			{SaleItemID: lineA, Quantity: 1, Amount: 999, Disposition: refundDomain.ReturnedToStock},
			{SaleItemID: lineB, Quantity: 2, Amount: 1, Disposition: refundDomain.Damaged},
		},
	}
	want, err := refundCommandFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	reordered := base
	reordered.Items = []refundDomain.ItemInput{base.Items[1], base.Items[0]}
	reordered.Items[0].Amount = 5000 // client amount is intentionally ignored
	if got, _ := refundCommandFingerprint(reordered); got != want {
		t.Fatal("item order or ignored client amount changed semantic fingerprint")
	}

	otherDrawer, otherSale, otherCorrection := uuid.New(), uuid.New(), uuid.New()
	cases := map[string]func(*RefundPaymentInput){
		"gym":         func(v *RefundPaymentInput) { v.GymID = uuid.New() },
		"actor":       func(v *RefundPaymentInput) { v.ActorUserID = uuid.New() },
		"payment":     func(v *RefundPaymentInput) { v.ParentPaymentID = uuid.New() },
		"reason":      func(v *RefundPaymentInput) { v.Reason = "Otra razón" },
		"method":      func(v *RefundPaymentInput) { v.Method = "transfer" },
		"drawer":      func(v *RefundPaymentInput) { v.CashDrawerID = &otherDrawer },
		"amount":      func(v *RefundPaymentInput) { v.Amount = 24.99 },
		"date":        func(v *RefundPaymentInput) { v.PaymentDate = v.PaymentDate.AddDate(0, 0, 1) },
		"revert":      func(v *RefundPaymentInput) { v.RevertMembership = true },
		"sale":        func(v *RefundPaymentInput) { v.SaleID = &otherSale },
		"quantity":    func(v *RefundPaymentInput) { v.Items[0].Quantity++ },
		"disposition": func(v *RefundPaymentInput) { v.Items[0].Disposition = refundDomain.NotReturned },
		"correction":  func(v *RefundPaymentInput) { v.CorrectionID = &otherCorrection },
		"kind":        func(v *RefundPaymentInput) { v.Kind = refundDomain.KindOvercollectionSettlement },
		"monetary":    func(v *RefundPaymentInput) { v.MonetaryAmount = 10 },
		"debt_cancel": func(v *RefundPaymentInput) { v.BalanceCancelled = 10 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := base
			changed.Items = append([]refundDomain.ItemInput(nil), base.Items...)
			mutate(&changed)
			got, err := refundCommandFingerprint(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("changed %s reused the original fingerprint", name)
			}
		})
	}
}
