//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
	billingRepoLite "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
)

func gymTestDay(offset int) time.Time {
	loc, _ := time.LoadLocation("America/Mexico_City")
	now := time.Now().In(loc).AddDate(0, 0, offset)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

func requireInvalidPaymentDate(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, billingErrors.ErrPaymentDateInvalid) {
		t.Fatalf("error = %v, want ErrPaymentDateInvalid", err)
	}
}

func TestMonetaryCommandsRejectFutureGymLocalDate(t *testing.T) {
	future := gymTestDay(1)

	t.Run("membership collection", func(t *testing.T) {
		f := setup(t)
		_, err := f.registerPayment().WithGyms(f.gymRepo).Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID, MembershipTypeID: f.planID,
			Method: "cash", PaymentDate: future, IdempotencyKey: "future-membership",
		})
		requireInvalidPaymentDate(t, err)
	})

	t.Run("product sale", func(t *testing.T) {
		f := setupSales(t)
		_, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", PaymentDate: future,
			IdempotencyKey: "future-sale",
			Items:          []billingApp.SaleLineInput{{ProductID: uuid.New(), Quantity: 1}},
		})
		requireInvalidPaymentDate(t, err)
	})

	t.Run("other income", func(t *testing.T) {
		f := setup(t)
		_, err := billingApp.NewRegisterOtherIncome(f.paymentRepo, f.folios, f.uow, f.recorder).
			WithGyms(f.gymRepo).Execute(context.Background(), billingApp.RegisterOtherIncomeInput{
			GymID: f.gymID, ActorUserID: f.ownerID, Amount: 100, Method: "transfer",
			Description: "Venta de equipo usado", PaymentDate: future, IdempotencyKey: "future-other-income",
		})
		requireInvalidPaymentDate(t, err)
	})

	t.Run("balance settlement", func(t *testing.T) {
		f := setup(t)
		_, err := billingApp.NewSettlePendingBalance(f.paymentRepo, f.folios, f.uow, f.recorder).
			WithGyms(f.gymRepo).Execute(context.Background(), billingApp.SettlePendingBalanceInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ParentPaymentID: uuid.New(), Amount: 100,
			Method: "cash", PaymentDate: future, IdempotencyKey: "future-settlement",
		})
		requireInvalidPaymentDate(t, err)
	})

	t.Run("refund", func(t *testing.T) {
		f := setup(t)
		_, err := billingApp.NewRefundPayment(f.paymentRepo, f.folios, f.memberSvc, f.uow, f.recorder).
			WithGyms(f.gymRepo).Execute(context.Background(), billingApp.RefundPaymentInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ParentPaymentID: uuid.New(), Amount: 100,
			Method: "cash", Reason: "Fecha inválida", PaymentDate: future, IdempotencyKey: "future-refund",
		})
		requireInvalidPaymentDate(t, err)
	})

	t.Run("sale correction collection", func(t *testing.T) {
		f := setupSales(t)
		_, err := financedCorrectionUC(f).Execute(context.Background(), billingApp.CorrectSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: uuid.New(),
			ExpectedVersion: 0, Reason: "Cobro adicional", MoneyResolution: "record_only",
			IncreaseResolution: "collect_now", CollectionMethod: "transfer", CollectionDate: future,
			IdempotencyKey: "future-correction-collection",
			Lines:          []billingApp.CorrectSaleLineInput{{ProductID: uuid.New(), Quantity: 1}},
		})
		requireInvalidPaymentDate(t, err)
	})

	t.Run("pending correction refund settlement", func(t *testing.T) {
		f := setupSales(t)
		_, err := financedCorrectionUC(f).SettlePending(context.Background(), billingApp.SettlePendingRefundInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", CorrectionID: uuid.New(),
			Method: "transfer", PaymentDate: future, IdempotencyKey: "future-pending-refund",
		})
		requireInvalidPaymentDate(t, err)
	})
}

func TestChildMoneyEventsCannotPrecedeTheirSourceCollection(t *testing.T) {
	sourceDay := gymTestDay(-2)
	beforeSource := sourceDay.AddDate(0, 0, -1)

	t.Run("balance settlement; same day remains valid", func(t *testing.T) {
		f := setup(t)
		root, err := f.registerPayment().WithGyms(f.gymRepo).Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID, MembershipTypeID: f.planID,
			Method: "cash", PaidNow: 300, PaymentDate: sourceDay, IdempotencyKey: "dated-root-for-settlement",
		})
		if err != nil {
			t.Fatal(err)
		}
		settle := billingApp.NewSettlePendingBalance(f.paymentRepo, f.folios, f.uow, f.recorder).WithGyms(f.gymRepo)
		_, err = settle.Execute(context.Background(), billingApp.SettlePendingBalanceInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ParentPaymentID: root.PaymentID, Amount: 100,
			Method: "transfer", PaymentDate: beforeSource, IdempotencyKey: "settlement-before-root",
		})
		requireInvalidPaymentDate(t, err)
		if _, err = settle.Execute(context.Background(), billingApp.SettlePendingBalanceInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ParentPaymentID: root.PaymentID, Amount: 100,
			Method: "transfer", PaymentDate: sourceDay, IdempotencyKey: "settlement-same-day",
		}); err != nil {
			t.Fatalf("same-day settlement: %v", err)
		}
	})

	t.Run("refund; same day remains valid", func(t *testing.T) {
		f := setup(t)
		root, err := f.registerPayment().WithGyms(f.gymRepo).Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID, MembershipTypeID: f.planID,
			Method: "cash", PaymentDate: sourceDay, IdempotencyKey: "dated-root-for-refund",
		})
		if err != nil {
			t.Fatal(err)
		}
		refund := billingApp.NewRefundPayment(f.paymentRepo, f.folios, f.memberSvc, f.uow, f.recorder).
			WithGyms(f.gymRepo).WithRefunds(billingRepoLite.NewRefundSQLiteRepository())
		_, err = refund.Execute(context.Background(), billingApp.RefundPaymentInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ParentPaymentID: root.PaymentID, Amount: root.Paid,
			Method: "cash", Reason: "Antes del cobro", PaymentDate: beforeSource, IdempotencyKey: "refund-before-root",
		})
		requireInvalidPaymentDate(t, err)
		if _, err = refund.Execute(context.Background(), billingApp.RefundPaymentInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ParentPaymentID: root.PaymentID, Amount: root.Paid,
			Method: "cash", Reason: "Mismo día", PaymentDate: sourceDay, IdempotencyKey: "refund-same-day",
		}); err != nil {
			t.Fatalf("same-day refund: %v", err)
		}
	})

	t.Run("sale correction collection; same day remains valid", func(t *testing.T) {
		f := setupSales(t)
		productID := f.seedProduct(t, "Agua", 10, 5)
		sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MemberID: &f.memberID, Method: "cash",
			PaymentDate: sourceDay, IdempotencyKey: "dated-sale-for-correction",
			Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		uc := financedCorrectionUC(f)
		detail, err := uc.Detail(context.Background(), f.gymID, sale.SaleID)
		if err != nil {
			t.Fatal(err)
		}
		input := billingApp.CorrectSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
			ExpectedVersion: 0, Reason: "Faltó una pieza", MoneyResolution: "record_only",
			IncreaseResolution: "collect_now", CollectionMethod: "transfer", CollectionDate: beforeSource,
			IdempotencyKey: "correction-collection-before-root",
			Lines: []billingApp.CorrectSaleLineInput{{
				SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 2,
			}},
		}
		_, err = uc.Execute(context.Background(), input)
		requireInvalidPaymentDate(t, err)
		input.CollectionDate = sourceDay
		input.IdempotencyKey = "correction-collection-same-day"
		if _, err = uc.Execute(context.Background(), input); err != nil {
			t.Fatalf("same-day correction collection: %v", err)
		}
	})

	t.Run("pending correction refund; same day remains valid", func(t *testing.T) {
		f := setupSales(t)
		productID := f.seedProduct(t, "Agua", 10, 5)
		sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", PaymentDate: sourceDay,
			IdempotencyKey: "dated-sale-for-pending-refund",
			Items:          []billingApp.SaleLineInput{{ProductID: productID, Quantity: 2}},
		})
		if err != nil {
			t.Fatal(err)
		}
		uc := financedCorrectionUC(f)
		detail, err := uc.Detail(context.Background(), f.gymID, sale.SaleID)
		if err != nil {
			t.Fatal(err)
		}
		corrected, err := uc.Execute(context.Background(), billingApp.CorrectSaleInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SaleID: sale.SaleID,
			ExpectedVersion: 0, Reason: "Se cobró una pieza de más", MoneyResolution: "refund_pending",
			IdempotencyKey: "create-pending-refund-for-date-test",
			Lines: []billingApp.CorrectSaleLineInput{{
				SaleItemID: &detail.Lines[0].SaleItemID, ProductID: productID, Quantity: 1,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = uc.SettlePending(context.Background(), billingApp.SettlePendingRefundInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", CorrectionID: corrected.CorrectionID,
			Method: "transfer", PaymentDate: beforeSource, IdempotencyKey: "pending-refund-before-root",
		})
		requireInvalidPaymentDate(t, err)
		if _, err = uc.SettlePending(context.Background(), billingApp.SettlePendingRefundInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", CorrectionID: corrected.CorrectionID,
			Method: "transfer", PaymentDate: sourceDay, IdempotencyKey: "pending-refund-same-day",
		}); err != nil {
			t.Fatalf("same-day pending correction refund: %v", err)
		}
	})
}

func TestSaleRefundDateValidationFlowsThroughWrapper(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 10, 2)
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", IdempotencyKey: "sale-for-future-refund",
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = financedRefundUC(f).Execute(context.Background(), billingApp.RefundSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, SaleID: sale.SaleID, Method: "cash",
		Reason: "Fecha futura", PaymentDate: gymTestDay(1), IdempotencyKey: "future-sale-refund",
		Items: []refundDomain.ItemInput{{
			SaleItemID: sale.Items[0].SaleItemID, Quantity: 1, Disposition: refundDomain.Damaged,
		}},
	})
	requireInvalidPaymentDate(t, err)
}
