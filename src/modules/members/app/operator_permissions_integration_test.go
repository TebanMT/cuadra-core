//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func operatorCtx(perms ...sharedDomain.Permission) context.Context {
	return sharedDomain.WithGrants(context.Background(), sharedDomain.OperatorGrants(perms))
}

func TestOperatorPermissions_MemberStatus(t *testing.T) {
	f := setupMembersFixture(t)
	typeID := f.createMembershipType(t, "Mensual")
	created, err := memApp.NewCreateMember(f.memberRepo, f.membershipR, f.mtRepo, f.uow, f.recorder).
		Execute(context.Background(), memApp.CreateMemberInput{
			GymID: f.gymID, ActorUserID: f.ownerID, FullName: "Juan", Phone: "+524421234567",
			MembershipTypeID: typeID, StartDate: time.Now().UTC(),
		})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	uc := memApp.NewToggleMemberStatus(f.memberRepo, f.uow, f.recorder)
	in := memApp.ToggleMemberStatusInput{GymID: f.gymID, ActorUserID: f.ownerID, MemberID: created.MemberID}

	for _, status := range []string{"inactive", "lost"} {
		in.NewStatus = status
		if _, err := uc.Execute(operatorCtx(), in); !errors.Is(err, sharedDomain.ErrPermissionDenied) {
			t.Errorf("operator → %s: err = %v, want denied", status, err)
		}
	}
	in.NewStatus = "inactive"
	if _, err := uc.Execute(operatorCtx(sharedDomain.PermMemberStatus), in); err != nil {
		t.Fatalf("operator with member_status: %v", err)
	}
	in.NewStatus = "active"
	if _, err := uc.Execute(operatorCtx(), in); err != nil {
		t.Fatalf("reactivating stays open: %v", err)
	}
}

func TestOperatorPermissions_FirstPaymentFeesAndDate(t *testing.T) {
	f := setupMembersFixture(t)
	if _, err := f.db.Exec(`UPDATE gyms SET charge_settings = ? WHERE id = ?`,
		`{"charges_enrollment":true,"enrollment_amount":150}`, f.gymID.String()); err != nil {
		t.Fatalf("charge settings: %v", err)
	}
	typeID := f.createMembershipType(t, "Mensual") // plan without its own fee → gym default 150
	uc := memApp.NewCreateMemberWithBilling(f.memberRepo, f.membershipR, f.mtRepo, f.paymentRepo, f.folios, f.uow, f.recorder).
		WithGyms(gymRepoLite.NewGymSQLiteRepository())
	phones := []string{"+524421000001", "+524421000002", "+524421000003", "+524421000004", "+524421000005"}
	n := 0
	input := func(mod func(*memApp.CreateMemberInput)) memApp.CreateMemberInput {
		in := memApp.CreateMemberInput{
			GymID: f.gymID, ActorUserID: f.ownerID, FullName: "Socio Nuevo", Phone: phones[n],
			MembershipTypeID: typeID, StartDate: time.Now().UTC(),
			ChargeFirstPayment: true, PaymentMethod: "cash",
			ChargeEnrollment: true, EnrollmentAmount: 150,
		}
		n++
		mod(&in)
		return in
	}

	if _, err := uc.Execute(operatorCtx(), input(func(in *memApp.CreateMemberInput) {
		in.ChargeEnrollment, in.EnrollmentAmount = false, 0
	})); !errors.Is(err, sharedDomain.ErrPermissionDenied) {
		t.Errorf("skipping the enrollment: err = %v, want denied", err)
	}
	if _, err := uc.Execute(operatorCtx(), input(func(in *memApp.CreateMemberInput) {
		in.StartDate = time.Now().UTC().AddDate(0, 0, -3)
	})); !errors.Is(err, sharedDomain.ErrPermissionDenied) {
		t.Errorf("backdated first payment: err = %v, want denied", err)
	}
	out, err := uc.Execute(operatorCtx(), input(func(*memApp.CreateMemberInput) {}))
	if err != nil {
		t.Fatalf("first payment as offered: %v", err)
	}
	if out.PaymentTotal != 650 {
		t.Errorf("first payment total = %v, want 650", out.PaymentTotal)
	}
	if _, err := uc.Execute(operatorCtx(), input(func(in *memApp.CreateMemberInput) {
		in.ChargeFirstPayment, in.PaymentMethod = false, ""
		in.StartDate = time.Now().UTC().AddDate(0, 0, -3)
	})); err != nil {
		t.Errorf("enrolling without a first payment stays open: %v", err)
	}
	if _, err := uc.Execute(operatorCtx(sharedDomain.PermDiscounts), input(func(in *memApp.CreateMemberInput) {
		in.ChargeEnrollment, in.EnrollmentAmount = false, 0
	})); err != nil {
		t.Errorf("operator with discounts may skip: %v", err)
	}
}
