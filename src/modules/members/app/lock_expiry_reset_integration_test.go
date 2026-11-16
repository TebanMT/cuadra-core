//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// "Volver al vencimiento calculado" quita lo que el dueño ajustó a mano
// (extender, establecer fecha) y conserva los días de una promoción.
func TestUC017_LockExpiry_ResetUndoesManualAdjustmentsOnly(t *testing.T) {
	f := setupMembersFixture(t)
	ctx := context.Background()
	typeID := f.createMembershipType(t, "Mensual")
	create := memApp.NewCreateMemberWithBilling(f.memberRepo, f.membershipR, f.mtRepo, f.paymentRepo, f.folios, f.uow, f.recorder)
	created, err := create.Execute(ctx, memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: "Juan", Phone: "+524421234567",
		MembershipTypeID: typeID, StartDate: time.Now().UTC(),
		ChargeFirstPayment: true,
		PaymentMethod:      "cash",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	expiry := func() string {
		t.Helper()
		var s string
		if err := f.db.Get(&s, `SELECT expiry_date FROM memberships WHERE id=?`, created.MembershipID.String()); err != nil {
			t.Fatalf("get expiry: %v", err)
		}
		return s
	}
	calculated, err := time.Parse("2006-01-02", expiry())
	if err != nil {
		t.Fatalf("parse expiry: %v", err)
	}
	day := func(n int) string { return calculated.AddDate(0, 0, n).Format("2006-01-02") }

	uc := memApp.NewLockMembershipExpiry(f.membershipR, f.adjustmentR, f.uow, f.recorder)
	lock := func(in memApp.LockMembershipExpiryInput) (*memApp.LockMembershipExpiryOutput, error) {
		in.GymID, in.ActorUserID, in.MembershipID = f.gymID, f.ownerID, created.MembershipID
		return uc.Execute(ctx, in)
	}

	if _, err := lock(memApp.LockMembershipExpiryInput{Mode: memApp.ModeExtendDays, Days: 14, Reason: "Cortesía por cierre"}); err != nil {
		t.Fatalf("extend: %v", err)
	}
	svc := memApp.NewMemberService(f.memberRepo, f.membershipR, f.mtRepo).WithAdjustments(f.adjustmentR)
	if err := f.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		return svc.ApplyMembershipAdjustment(ctx, tx, memApp.ApplyMembershipAdjustmentInput{
			GymID: f.gymID, MembershipID: created.MembershipID, Days: 7,
			Reason: membership.PromoAdjustmentReasonPrefix + "Buen Fin", ActorUserID: f.ownerID,
		}, time.Now().UTC())
	}); err != nil {
		t.Fatalf("promo: %v", err)
	}
	target, _ := time.Parse("2006-01-02", day(31))
	if _, err := lock(memApp.LockMembershipExpiryInput{Mode: memApp.ModeSetDate, NewExpiry: target, Reason: "Fecha acordada"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := expiry(); got != day(31) {
		t.Fatalf("before reset expiry = %s, want %s", got, day(31))
	}

	out, err := lock(memApp.LockMembershipExpiryInput{Mode: memApp.ModeReset, Reason: "Me equivoqué de socio"})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := out.NewExpiry.Format("2006-01-02"); got != day(7) {
		t.Errorf("reset new expiry = %s, want %s (calculated + 7 promo days)", got, day(7))
	}
	if out.DaysAdded != -24 {
		t.Errorf("reset days = %d, want -24", out.DaysAdded)
	}
	if got := expiry(); got != day(7) {
		t.Errorf("db expiry = %s, want %s", got, day(7))
	}

	// Nothing manual left: a second reset is rejected and changes nothing.
	if _, err := lock(memApp.LockMembershipExpiryInput{Mode: memApp.ModeReset, Reason: "Otra vez por si acaso"}); !errors.Is(err, memErrors.ErrNoManualAdjustments) {
		t.Fatalf("second reset err = %v, want ErrNoManualAdjustments", err)
	}
	if got := expiry(); got != day(7) {
		t.Errorf("expiry after rejected reset = %s, want %s", got, day(7))
	}
	var nAdj int
	if err := f.db.Get(&nAdj, `SELECT COUNT(*) FROM membership_adjustments WHERE membership_id=?`, created.MembershipID.String()); err != nil {
		t.Fatalf("count adj: %v", err)
	}
	if nAdj != 4 {
		t.Errorf("adjustments = %d, want 4 (extend, promo, set, reset)", nAdj)
	}
}
