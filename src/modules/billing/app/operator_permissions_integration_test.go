//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Operator permissions on charges (UC-007): the use cases read the actor's
// grants from the context the HTTP middleware fills.

func operatorCtx(perms ...sharedDomain.Permission) context.Context {
	return sharedDomain.WithGrants(context.Background(), sharedDomain.OperatorGrants(perms))
}

func assertDenied(t *testing.T, err error, want sharedDomain.Permission) {
	t.Helper()
	if !errors.Is(err, sharedDomain.ErrPermissionDenied) {
		t.Fatalf("err = %v, want permission denied (%s)", err, want)
	}
	var ce sharedDomain.CustomError
	if !errors.As(err, &ce) || ce.ErrorCode != sharedDomain.CodeForbidden {
		t.Fatalf("err = %#v, want CustomError with CodeForbidden", err)
	}
	if got, _ := ce.Data["permission"].(string); got != string(want) {
		t.Errorf("denied permission = %q, want %q", got, want)
	}
}

func daysAgo(n int) time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -n)
}

func TestOperatorPermissions_MembershipPaymentCreditAndDiscount(t *testing.T) {
	f := setup(t)
	uc := f.registerPayment().WithGyms(f.gymRepo)
	base := billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
	}

	partial := base
	partial.PaidNow = 100
	_, err := uc.Execute(operatorCtx(), partial)
	assertDenied(t, err, sharedDomain.PermCredit)
	if _, err := uc.Execute(operatorCtx(sharedDomain.PermCredit), partial); err != nil {
		t.Fatalf("operator with credit: %v", err)
	}

	reason := "Cliente frecuente"
	discounted := base
	discounted.Discount, discounted.DiscountReason = 50, &reason
	_, err = uc.Execute(operatorCtx(sharedDomain.PermCredit), discounted)
	assertDenied(t, err, sharedDomain.PermDiscounts)
	if _, err := uc.Execute(operatorCtx(sharedDomain.PermDiscounts), discounted); err != nil {
		t.Fatalf("operator with discounts: %v", err)
	}

	backdated := base
	backdated.PaymentDate = daysAgo(3)
	_, err = uc.Execute(operatorCtx(), backdated)
	assertDenied(t, err, sharedDomain.PermDiscounts)
	// No grants in the context = internal caller; the owner path is the same.
	if _, err := uc.Execute(context.Background(), backdated); err != nil {
		t.Fatalf("backdated without grants: %v", err)
	}
	if _, err := uc.Execute(operatorCtx(), base); err != nil {
		t.Fatalf("plain full payment today must stay open: %v", err)
	}
}

func TestOperatorPermissions_MembershipPaymentDueFees(t *testing.T) {
	f := setup(t)
	// The gym charges enrollment; the plan's own fee (100) wins over the
	// gym default (150), as reception shows it.
	if _, err := f.db.Exec(`UPDATE gyms SET charge_settings = ? WHERE id = ?`,
		`{"charges_enrollment":true,"enrollment_amount":150}`, f.gymID.String()); err != nil {
		t.Fatalf("charge settings: %v", err)
	}
	uc := f.registerPayment().WithGyms(f.gymRepo)
	base := billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
	}
	no, yes := false, true

	skip := base
	skip.ChargeEnrollment = &no
	_, err := uc.Execute(operatorCtx(), skip)
	assertDenied(t, err, sharedDomain.PermDiscounts)

	cheaper := base
	cheaper.ChargeEnrollment, cheaper.EnrollmentAmount = &yes, 40
	_, err = uc.Execute(operatorCtx(), cheaper)
	assertDenied(t, err, sharedDomain.PermDiscounts)

	if _, err := uc.Execute(operatorCtx(sharedDomain.PermDiscounts), skip); err != nil {
		t.Fatalf("operator with discounts may skip: %v", err)
	}

	asOffered := base
	asOffered.ChargeEnrollment, asOffered.EnrollmentAmount = &yes, 100
	out, err := uc.Execute(operatorCtx(), asOffered)
	if err != nil {
		t.Fatalf("charging the offered fee must stay open: %v", err)
	}
	if !out.EnrollmentChrg || out.Total != 600 {
		t.Errorf("charged enrollment=%v total=%v, want true/600", out.EnrollmentChrg, out.Total)
	}
	// Enrollment is paid now: forcing it again collects more, skipping is fine.
	if _, err := uc.Execute(operatorCtx(), skip); err != nil {
		t.Fatalf("skipping a fee that is no longer due: %v", err)
	}
}

func TestOperatorPermissions_SaleCreditDiscountAndDate(t *testing.T) {
	f := setupSales(t)
	p := f.seedProduct(t, "Agua", 20, 10)
	mid := f.memberID
	uc := f.registerSale()
	line := []billingApp.SaleLineInput{{ProductID: p, Quantity: 1}}

	paid := 5.0
	_, err := uc.Execute(operatorCtx(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		MemberID: &mid, Paid: &paid, Items: line,
	})
	assertDenied(t, err, sharedDomain.PermCredit)

	_, err = uc.Execute(operatorCtx(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		Discount: 5, Items: line,
	})
	assertDenied(t, err, sharedDomain.PermDiscounts)

	_, err = uc.Execute(operatorCtx(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		PaymentDate: daysAgo(2), Items: line,
	})
	assertDenied(t, err, sharedDomain.PermDiscounts)

	if _, err := uc.Execute(operatorCtx(sharedDomain.PermCredit), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		MemberID: &mid, Paid: &paid, Items: line,
	}); err != nil {
		t.Fatalf("operator with credit: %v", err)
	}
	if _, err := uc.Execute(operatorCtx(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", Items: line,
	}); err != nil {
		t.Fatalf("plain sale must stay open: %v", err)
	}
}
