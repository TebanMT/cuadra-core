package promotion

import (
	"testing"

	"github.com/google/uuid"
)

func TestCalculate_Percent(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "10%", Kind: KindPercent, Value: ptrFloat(10), AppliesTo: AppliesToAny,
	}, now())
	got := Calculate(p, CalcInput{Subtotal: 500})
	if got.Discount != 50 {
		t.Errorf("10%% de 500 = 50, got %v", got.Discount)
	}
}

func TestCalculate_Percent100(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Gratis", Kind: KindPercent, Value: ptrFloat(100), AppliesTo: AppliesToAny,
	}, now())
	got := Calculate(p, CalcInput{Subtotal: 500})
	if got.Discount != 500 {
		t.Errorf("100%% de 500 = 500, got %v", got.Discount)
	}
}

func TestCalculate_FixedAmountClampedToSubtotal(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Bonazo", Kind: KindFixedAmount, Value: ptrFloat(900), AppliesTo: AppliesToAny,
	}, now())
	got := Calculate(p, CalcInput{Subtotal: 500})
	if got.Discount != 500 {
		t.Errorf("fixed > subtotal debe clampear a subtotal, got %v", got.Discount)
	}
}

func TestCalculate_FreeEnrollmentApplies(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Sin insc", Kind: KindFreeEnrollment, AppliesTo: AppliesToMembership,
	}, now())
	got := Calculate(p, CalcInput{Subtotal: 800, EnrollmentFee: 300, HasEnrollment: true})
	if got.Discount != 300 {
		t.Errorf("free_enrollment debe descontar enrollment_fee, got %v", got.Discount)
	}
}

func TestCalculate_FreeEnrollmentNoOpWhenNotCharged(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Sin insc", Kind: KindFreeEnrollment, AppliesTo: AppliesToMembership,
	}, now())
	got := Calculate(p, CalcInput{Subtotal: 500, EnrollmentFee: 300, HasEnrollment: false})
	if got.Discount != 0 {
		t.Errorf("no enrollment → no-op, got %v", got.Discount)
	}
}

func TestCalculate_ExtraDays(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Días gratis", Kind: KindExtraDays, Value: ptrFloat(5), AppliesTo: AppliesToMembership,
	}, now())
	got := Calculate(p, CalcInput{Subtotal: 500})
	if got.ExtraDays != 5 || got.Discount != 0 {
		t.Errorf("extra_days = %d / discount = %v", got.ExtraDays, got.Discount)
	}
}

func TestCalculate_CompanionMemberships(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "2x1", Kind: KindCompanionMemberships, CompanionCount: ptrInt(1),
		AppliesTo: AppliesToMembership,
	}, now())
	got := Calculate(p, CalcInput{Subtotal: 500})
	if got.CompanionCount != 1 || got.Discount != 0 {
		t.Errorf("companion = %d / discount = %v", got.CompanionCount, got.Discount)
	}
}

func TestCalculate_NilSafe(t *testing.T) {
	if got := Calculate(nil, CalcInput{Subtotal: 500}); got.Discount != 0 || got.ExtraDays != 0 {
		t.Errorf("nil promo debe devolver zero result, got %+v", got)
	}
}

func TestNewApplied_Snapshot(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Verano 25", Kind: KindPercent, Value: ptrFloat(25), AppliesTo: AppliesToMembership,
	}, now())
	res := Calculate(p, CalcInput{Subtotal: 400})
	ap := NewApplied(uuid.New(), p.GymID, p, res, NewAppliedParams{
		PromotionID: p.ID, PaymentID: uuid.New(), AppliedByUserID: uuid.New(),
	}, now())
	if ap.PromotionNameSnapshot != "Verano 25" || ap.KindSnapshot != KindPercent {
		t.Errorf("snapshot vacío: %+v", ap)
	}
	if ap.ValueSnapshot == nil || *ap.ValueSnapshot != 25 {
		t.Errorf("value snapshot perdido")
	}
	if ap.DiscountAmount != 100 {
		t.Errorf("discount snapshot = %v, want 100", ap.DiscountAmount)
	}
}
