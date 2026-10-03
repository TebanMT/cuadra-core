//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	billingRepoLite "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
)

func TestCorrectPayment_MembershipCaptureFactsAreAuditedAndReplayExactly(t *testing.T) {
	f := setup(t)
	chargeEnrollment := false
	registered, err := f.registerPayment().Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash", PaidNow: 400, ChargeEnrollment: &chargeEnrollment, IdempotencyKey: "membership-before-correction",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := f.uow.Query(context.Background())
	payment, err := f.paymentRepo.GetByID(tx, registered.PaymentID)
	if err != nil {
		t.Fatal(err)
	}
	amount := 450.0
	correctedDate := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	uc := billingApp.NewCorrectPayment(f.paymentRepo, billingRepoLite.NewPaymentCorrectionSQLiteRepository(), nil, f.uow, f.recorder)
	in := billingApp.CorrectPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PaymentID: payment.ID,
		ExpectedVersion: payment.Version, Reason: "El recibo decía efectivo y $400; fue transferencia de $450",
		Amount: &amount, PaymentMethod: "transfer", PaymentDate: correctedDate, IdempotencyKey: "correct-membership-capture",
	}
	first, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent replay differs: first=%+v second=%+v", first, second)
	}
	if first.Before.Amount != 400 || first.Before.BalancePending != 100 || first.After.Amount != 450 ||
		first.After.RecognizedAmount != 450 || first.After.BalancePending != 50 || first.After.PaymentMethod != "transfer" ||
		first.After.PaymentDate != "2026-08-23" || first.ServiceEffectsChanged {
		t.Fatalf("correction effect=%+v", first)
	}
	var stored struct {
		Amount, Recognized, Balance int64
		Method                      string
		Date                        string
		Version                     int
	}
	if err := f.db.Get(&stored, `SELECT amount,recognized_amount AS recognized,balance_pending AS balance,
		payment_method AS method,payment_date AS date,version FROM payments WHERE id=?`, payment.ID.String()); err != nil {
		t.Fatal(err)
	}
	if stored.Amount != 45000 || stored.Recognized != 45000 || stored.Balance != 5000 || stored.Method != "transfer" || stored.Date != "2026-08-23" {
		t.Fatalf("stored payment=%+v", stored)
	}
	history, err := uc.History(context.Background(), f.gymID, payment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].ID != first.CorrectionID || history[0].Before.Amount != 400 || history[0].After.Amount != 450 {
		t.Fatalf("history=%+v", history)
	}
	changed := in
	changedAmount := 425.0
	changed.Amount = &changedAmount
	if _, err := uc.Execute(context.Background(), changed); !errors.Is(err, billingErrors.ErrIdempotencyKeyConflict) {
		t.Fatalf("changed payload with reused key error=%v", err)
	}
}

func TestCorrectPayment_RejectsOperatorAndProductSpecificConcept(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 10, 2)
	sale, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", IdempotencyKey: "sale-for-generic-rejection",
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := f.uow.Query(context.Background())
	payment, _ := f.paymentRepo.GetByID(tx, sale.PaymentID)
	amount := 9.0
	uc := billingApp.NewCorrectPayment(f.paymentRepo, billingRepoLite.NewPaymentCorrectionSQLiteRepository(), nil, f.uow, f.recorder)
	base := billingApp.CorrectPaymentInput{GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "operator", PaymentID: payment.ID,
		ExpectedVersion: payment.Version, Reason: "Monto mal capturado", Amount: &amount, IdempotencyKey: "generic-product"}
	if _, err := uc.Execute(context.Background(), base); !errors.Is(err, billingErrors.ErrPaymentCorrectionForbidden) {
		t.Fatalf("operator error=%v", err)
	}
	base.ActorRole = "owner"
	if _, err := uc.Execute(context.Background(), base); !errors.Is(err, billingErrors.ErrPaymentCorrectionUnsupported) {
		t.Fatalf("product generic correction error=%v", err)
	}
}

func TestCorrectPayment_AnnulsNonexistentOtherIncomeWithoutInventingRefund(t *testing.T) {
	f := setup(t)
	registered, err := billingApp.NewRegisterOtherIncome(f.paymentRepo, f.folios, f.uow, f.recorder).
		WithGyms(f.gymRepo).Execute(context.Background(), billingApp.RegisterOtherIncomeInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Amount: 350, Method: "cash",
		Description: "Venta de caminadora usada", IdempotencyKey: "other-income-to-annul",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := f.uow.Query(context.Background())
	payment, err := f.paymentRepo.GetByID(tx, registered.PaymentID)
	if err != nil {
		t.Fatal(err)
	}
	uc := billingApp.NewCorrectPayment(f.paymentRepo, billingRepoLite.NewPaymentCorrectionSQLiteRepository(), nil, f.uow, f.recorder).
		WithGyms(f.gymRepo)
	in := billingApp.CorrectPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PaymentID: payment.ID,
		ExpectedVersion: payment.Version, Reason: "La venta nunca ocurrió", Annul: true,
		IdempotencyKey: "annul-other-capture",
	}
	first, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := uc.Execute(context.Background(), in)
	if err != nil || !reflect.DeepEqual(first, replayed) {
		t.Fatalf("replay=%+v err=%v, first=%+v", replayed, err, first)
	}
	if !first.Annulled || first.Before.Annulled || !first.After.Annulled || first.ServiceEffectsChanged ||
		first.After.Amount != 350 || first.After.RecognizedAmount != 350 {
		t.Fatalf("annul effect=%+v", first)
	}
	var stored struct {
		Amount, Recognized int64
		Version            int
		Deleted            int `db:"deleted"`
	}
	if err := f.db.Get(&stored, `SELECT amount,recognized_amount AS recognized,version,
		(deleted_at IS NOT NULL) AS deleted FROM payments WHERE id=?`, payment.ID.String()); err != nil {
		t.Fatal(err)
	}
	if stored.Amount != 35000 || stored.Recognized != 35000 || stored.Version != payment.Version+1 || stored.Deleted != 1 {
		t.Fatalf("stored tombstone=%+v", stored)
	}
	tx, _ = f.uow.Query(context.Background())
	if _, err := f.paymentRepo.GetByID(tx, payment.ID); !errors.Is(err, billingErrors.ErrPaymentNotFound) {
		t.Fatalf("annulled payment must leave live reads: %v", err)
	}
	history, err := uc.History(context.Background(), f.gymID, payment.ID)
	if err != nil || len(history) != 1 || !history[0].Annulled {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	var liveIncome, refunds int
	if err := f.db.Get(&liveIncome, `SELECT COALESCE(SUM(recognized_amount),0) FROM payments
		WHERE gym_id=? AND concept='other' AND deleted_at IS NULL`, f.gymID.String()); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Get(&refunds, `SELECT COUNT(*) FROM refunds WHERE gym_id=? AND deleted_at IS NULL`, f.gymID.String()); err != nil {
		t.Fatal(err)
	}
	if liveIncome != 0 || refunds != 0 {
		t.Fatalf("annul must remove income without refund: live_income=%d refunds=%d", liveIncome, refunds)
	}
}

func TestCorrectPayment_RejectsMembershipAnnulAndFutureLocalDate(t *testing.T) {
	f := setup(t)
	chargeEnrollment := false
	registered, err := f.registerPayment().Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "transfer", PaidNow: 400, ChargeEnrollment: &chargeEnrollment,
		IdempotencyKey: "membership-annul-rejection",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := f.uow.Query(context.Background())
	payment, _ := f.paymentRepo.GetByID(tx, registered.PaymentID)
	uc := billingApp.NewCorrectPayment(f.paymentRepo, billingRepoLite.NewPaymentCorrectionSQLiteRepository(), nil, f.uow, f.recorder).
		WithGyms(f.gymRepo)
	base := billingApp.CorrectPaymentInput{GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner",
		PaymentID: payment.ID, ExpectedVersion: payment.Version, Reason: "Captura inexistente",
		Annul: true, IdempotencyKey: "cannot-annul-membership"}
	if _, err := uc.Execute(context.Background(), base); !errors.Is(err, billingErrors.ErrPaymentCorrectionAnnulMembership) {
		t.Fatalf("membership annul error=%v", err)
	}
	future := time.Now().UTC().AddDate(0, 0, 2)
	base.Annul = false
	base.PaymentDate = future
	base.IdempotencyKey = "future-membership-correction"
	if _, err := uc.Execute(context.Background(), base); !errors.Is(err, billingErrors.ErrPaymentDateInvalid) {
		t.Fatalf("future local date error=%v", err)
	}
}
