package promotion

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	promoErrors "github.com/cuadra/cuadra-core/src/modules/promotions/domain/errors"
)

func ptrFloat(v float64) *float64 { return &v }
func ptrInt(v int) *int           { return &v }
func ptrStr(v string) *string     { return &v }
func ptrTime(t time.Time) *time.Time {
	return &t
}

func now() time.Time { return time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC) }

func TestNew_PercentValid(t *testing.T) {
	p, err := New(uuid.New(), uuid.New(), NewParams{
		Name: "Verano 25", Kind: KindPercent, Value: ptrFloat(25), AppliesTo: AppliesToMembership,
	}, now())
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.BuyN != 1 {
		t.Errorf("buy_n default = 1, got %d", p.BuyN)
	}
	if !p.Active {
		t.Errorf("active default = true")
	}
}

func TestNew_PercentOutOfRange(t *testing.T) {
	_, err := New(uuid.New(), uuid.New(), NewParams{
		Name: "Mal", Kind: KindPercent, Value: ptrFloat(150), AppliesTo: AppliesToAny,
	}, now())
	if !errors.Is(err, promoErrors.ErrInvalidPromotionValue) {
		t.Fatalf("want ErrInvalidPromotionValue, got %v", err)
	}
}

func TestNew_FixedAmountZero(t *testing.T) {
	_, err := New(uuid.New(), uuid.New(), NewParams{
		Name: "Cero", Kind: KindFixedAmount, Value: ptrFloat(0), AppliesTo: AppliesToAny,
	}, now())
	if !errors.Is(err, promoErrors.ErrInvalidPromotionValue) {
		t.Fatalf("want ErrInvalidPromotionValue, got %v", err)
	}
}

func TestNew_FreeEnrollmentRejectsValue(t *testing.T) {
	_, err := New(uuid.New(), uuid.New(), NewParams{
		Name: "Free", Kind: KindFreeEnrollment, Value: ptrFloat(100), AppliesTo: AppliesToMembership,
	}, now())
	if !errors.Is(err, promoErrors.ErrInvalidPromotionValue) {
		t.Fatalf("want ErrInvalidPromotionValue, got %v", err)
	}
}

func TestNew_CompanionRequiresCount(t *testing.T) {
	_, err := New(uuid.New(), uuid.New(), NewParams{
		Name: "2x1", Kind: KindCompanionMemberships, AppliesTo: AppliesToMembership,
	}, now())
	if !errors.Is(err, promoErrors.ErrInvalidCompanionCount) {
		t.Fatalf("want ErrInvalidCompanionCount, got %v", err)
	}
}

func TestNew_ShortName(t *testing.T) {
	_, err := New(uuid.New(), uuid.New(), NewParams{
		Name: "ab", Kind: KindPercent, Value: ptrFloat(10), AppliesTo: AppliesToAny,
	}, now())
	if !errors.Is(err, promoErrors.ErrInvalidPromotionName) {
		t.Fatalf("want ErrInvalidPromotionName, got %v", err)
	}
}

func TestNew_InvalidDates(t *testing.T) {
	from := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	_, err := New(uuid.New(), uuid.New(), NewParams{
		Name: "Mal fechas", Kind: KindPercent, Value: ptrFloat(10), AppliesTo: AppliesToAny,
		ValidFrom: &from, ValidUntil: &until,
	}, now())
	if !errors.Is(err, promoErrors.ErrInvalidPromotionDates) {
		t.Fatalf("want ErrInvalidPromotionDates, got %v", err)
	}
}

func TestIsCurrentlyValid_Window(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Win", Kind: KindPercent, Value: ptrFloat(10), AppliesTo: AppliesToAny,
		ValidFrom:  ptrTime(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)),
		ValidUntil: ptrTime(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)),
	}, now())
	if err := p.IsCurrentlyValid(time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)); !errors.Is(err, promoErrors.ErrPromotionNotYetValid) {
		t.Errorf("before window: %v", err)
	}
	if err := p.IsCurrentlyValid(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Errorf("in window: %v", err)
	}
	if err := p.IsCurrentlyValid(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, promoErrors.ErrPromotionExpired) {
		t.Errorf("after window: %v", err)
	}
	p.Deactivate(now())
	if err := p.IsCurrentlyValid(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)); !errors.Is(err, promoErrors.ErrPromotionInactive) {
		t.Errorf("inactive: %v", err)
	}
}

func TestAppliesToTarget(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Any", Kind: KindPercent, Value: ptrFloat(10), AppliesTo: AppliesToAny,
	}, now())
	if !p.AppliesToTarget(AppliesToMembership) || !p.AppliesToTarget(AppliesToSale) {
		t.Errorf("any debe matchear ambos")
	}
	p2, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Sale", Kind: KindPercent, Value: ptrFloat(10), AppliesTo: AppliesToSale,
	}, now())
	if p2.AppliesToTarget(AppliesToMembership) {
		t.Errorf("sale no debe matchear membership")
	}
}

func TestNormalizedCode(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Cup", Kind: KindPercent, Value: ptrFloat(10), AppliesTo: AppliesToAny,
		Code: ptrStr("  Verano2026 "),
	}, now())
	if got := p.NormalizedCode(); got != "verano2026" {
		t.Errorf("normalized = %q", got)
	}
}

func TestUpdate_BumpsVersion(t *testing.T) {
	p, _ := New(uuid.New(), uuid.New(), NewParams{
		Name: "Original", Kind: KindPercent, Value: ptrFloat(10), AppliesTo: AppliesToAny,
	}, now())
	v0 := p.Version
	if err := p.Update(NewParams{
		Name: "Editado", Kind: KindPercent, Value: ptrFloat(20), AppliesTo: AppliesToAny,
	}, now().Add(time.Hour)); err != nil {
		t.Fatalf("update: %v", err)
	}
	if p.Version != v0+1 {
		t.Errorf("version no bumpeó: %d → %d", v0, p.Version)
	}
	if p.Name != "Editado" || *p.Value != 20 {
		t.Errorf("update no aplicó campos")
	}
}
