package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	promoErrors "github.com/cuadra/cuadra-core/src/modules/promotions/domain/errors"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// fakeTx — Transaction sin estado real (los repos fake no la usan).
type fakeTx struct{}

func (fakeTx) Execute(fn func(tx sharedDomain.Transaction) error) error { return fn(fakeTx{}) }

// fakePromotionRepo — minimal in-memory para apply tests.
type fakePromotionRepo struct {
	byID   map[uuid.UUID]*promoDomain.Promotion
	byCode map[string]*promoDomain.Promotion
}

func newFakeRepo() *fakePromotionRepo {
	return &fakePromotionRepo{byID: map[uuid.UUID]*promoDomain.Promotion{}, byCode: map[string]*promoDomain.Promotion{}}
}

func (r *fakePromotionRepo) put(p *promoDomain.Promotion) {
	r.byID[p.ID] = p
	if p.Code != nil {
		r.byCode[strings.ToLower(*p.Code)] = p
	}
}

func (r *fakePromotionRepo) Create(tx sharedDomain.Transaction, p *promoDomain.Promotion) (*promoDomain.Promotion, error) {
	r.put(p)
	return p, nil
}
func (r *fakePromotionRepo) Update(tx sharedDomain.Transaction, p *promoDomain.Promotion) (*promoDomain.Promotion, error) {
	r.put(p)
	return p, nil
}
func (r *fakePromotionRepo) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*promoDomain.Promotion, error) {
	if p, ok := r.byID[id]; ok {
		return p, nil
	}
	return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionNotFound, "")
}
func (r *fakePromotionRepo) GetByCode(tx sharedDomain.Transaction, gymID uuid.UUID, codeLower string) (*promoDomain.Promotion, error) {
	if p, ok := r.byCode[codeLower]; ok && p.GymID == gymID {
		return p, nil
	}
	return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionCodeNotFound, "")
}
func (r *fakePromotionRepo) List(tx sharedDomain.Transaction, f promoRepo.ListFilter) ([]*promoDomain.Promotion, error) {
	return nil, nil
}
func (r *fakePromotionRepo) ExistsByCode(tx sharedDomain.Transaction, gymID uuid.UUID, codeLower string, excludeID *uuid.UUID) (bool, error) {
	p, ok := r.byCode[codeLower]
	if !ok || p.GymID != gymID {
		return false, nil
	}
	if excludeID != nil && p.ID == *excludeID {
		return false, nil
	}
	return true, nil
}

type fakeAppliedRepo struct {
	rows          []*promoDomain.AppliedPromotion
	byPromotion   map[uuid.UUID]int
	byPromoMember map[string]int
}

func newAppliedRepo() *fakeAppliedRepo {
	return &fakeAppliedRepo{byPromotion: map[uuid.UUID]int{}, byPromoMember: map[string]int{}}
}

func keyPM(p, m uuid.UUID) string { return p.String() + ":" + m.String() }

func (r *fakeAppliedRepo) Create(tx sharedDomain.Transaction, ap *promoDomain.AppliedPromotion) (*promoDomain.AppliedPromotion, error) {
	r.rows = append(r.rows, ap)
	r.byPromotion[ap.PromotionID]++
	if ap.MemberID != nil {
		r.byPromoMember[keyPM(ap.PromotionID, *ap.MemberID)]++
	}
	return ap, nil
}
func (r *fakeAppliedRepo) CountByPromotion(tx sharedDomain.Transaction, promotionID uuid.UUID) (int, error) {
	return r.byPromotion[promotionID], nil
}
func (r *fakeAppliedRepo) CountByPromotionAndMember(tx sharedDomain.Transaction, promotionID, memberID uuid.UUID) (int, error) {
	return r.byPromoMember[keyPM(promotionID, memberID)], nil
}
func (r *fakeAppliedRepo) SummaryByMonth(tx sharedDomain.Transaction, gymID uuid.UUID, monthStart, monthEnd time.Time) ([]promoRepo.AppliedSummary, error) {
	return nil, nil
}

func ptrF(v float64) *float64 { return &v }
func ptrI(v int) *int         { return &v }
func ptrT(t time.Time) *time.Time {
	return &t
}

func newGymPromo(t *testing.T, kind string, opts ...func(*promoDomain.NewParams)) (*promoDomain.Promotion, uuid.UUID) {
	t.Helper()
	gymID := uuid.New()
	params := promoDomain.NewParams{
		Name: "Test", Kind: kind, AppliesTo: promoDomain.AppliesToMembership,
	}
	switch kind {
	case promoDomain.KindPercent:
		params.Value = ptrF(25)
	case promoDomain.KindFixedAmount:
		params.Value = ptrF(100)
	case promoDomain.KindExtraDays:
		params.Value = ptrF(7)
	case promoDomain.KindCompanionMemberships:
		params.CompanionCount = ptrI(1)
	}
	for _, o := range opts {
		o(&params)
	}
	p, err := promoDomain.New(uuid.New(), gymID, params, time.Now())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	return p, gymID
}

func TestApply_PercentApplied(t *testing.T) {
	p, gymID := newGymPromo(t, promoDomain.KindPercent)
	repo := newFakeRepo()
	repo.put(p)
	uc := NewApplyPromotion(repo, newAppliedRepo())
	out, err := uc.Execute(context.Background(), fakeTx{}, ApplyPromotionInput{
		GymID:       gymID,
		ActorUserID: uuid.New(),
		PromotionID: &p.ID,
		PaymentID:   uuid.New(),
		Target:      promoDomain.AppliesToMembership,
		Subtotal:    400,
		Today:       time.Now(),
	}, time.Now())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.Discount != 100 {
		t.Errorf("25%% de 400 = 100, got %v", out.Discount)
	}
}

func TestApply_InactiveRejected(t *testing.T) {
	p, gymID := newGymPromo(t, promoDomain.KindPercent)
	p.Deactivate(time.Now())
	repo := newFakeRepo()
	repo.put(p)
	uc := NewApplyPromotion(repo, newAppliedRepo())
	_, err := uc.Execute(context.Background(), fakeTx{}, ApplyPromotionInput{
		GymID: gymID, ActorUserID: uuid.New(),
		PromotionID: &p.ID, PaymentID: uuid.New(),
		Target: promoDomain.AppliesToMembership, Subtotal: 400, Today: time.Now(),
	}, time.Now())
	if err == nil || !errors.Is(err, promoErrors.ErrPromotionInactive) {
		t.Fatalf("want inactive, got %v", err)
	}
}

func TestApply_ExpiredRejected(t *testing.T) {
	expired := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p, gymID := newGymPromo(t, promoDomain.KindPercent, func(np *promoDomain.NewParams) {
		np.ValidUntil = ptrT(expired)
	})
	repo := newFakeRepo()
	repo.put(p)
	uc := NewApplyPromotion(repo, newAppliedRepo())
	_, err := uc.Execute(context.Background(), fakeTx{}, ApplyPromotionInput{
		GymID: gymID, ActorUserID: uuid.New(),
		PromotionID: &p.ID, PaymentID: uuid.New(),
		Target: promoDomain.AppliesToMembership, Subtotal: 400,
		Today: time.Now(),
	}, time.Now())
	if err == nil || !errors.Is(err, promoErrors.ErrPromotionExpired) {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestApply_UsageLimitExceeded(t *testing.T) {
	p, gymID := newGymPromo(t, promoDomain.KindPercent, func(np *promoDomain.NewParams) {
		max := 1
		np.MaxUsesTotal = &max
	})
	repo := newFakeRepo()
	repo.put(p)
	applied := newAppliedRepo()
	applied.byPromotion[p.ID] = 1
	uc := NewApplyPromotion(repo, applied)
	_, err := uc.Execute(context.Background(), fakeTx{}, ApplyPromotionInput{
		GymID: gymID, ActorUserID: uuid.New(),
		PromotionID: &p.ID, PaymentID: uuid.New(),
		Target: promoDomain.AppliesToMembership, Subtotal: 400, Today: time.Now(),
	}, time.Now())
	if err == nil || !errors.Is(err, promoErrors.ErrPromotionUsageLimitExceeded) {
		t.Fatalf("want usage limit, got %v", err)
	}
}

func TestApply_PerMemberLimit(t *testing.T) {
	p, gymID := newGymPromo(t, promoDomain.KindPercent, func(np *promoDomain.NewParams) {
		max := 1
		np.MaxUsesPerMember = &max
	})
	memberID := uuid.New()
	repo := newFakeRepo()
	repo.put(p)
	applied := newAppliedRepo()
	applied.byPromoMember[keyPM(p.ID, memberID)] = 1
	uc := NewApplyPromotion(repo, applied)
	_, err := uc.Execute(context.Background(), fakeTx{}, ApplyPromotionInput{
		GymID: gymID, ActorUserID: uuid.New(),
		PromotionID: &p.ID, PaymentID: uuid.New(), MemberID: &memberID,
		Target: promoDomain.AppliesToMembership, Subtotal: 400, Today: time.Now(),
	}, time.Now())
	if err == nil || !errors.Is(err, promoErrors.ErrPromotionUsageLimitPerMember) {
		t.Fatalf("want per-member limit, got %v", err)
	}
}

func TestApply_TargetMismatch(t *testing.T) {
	p, gymID := newGymPromo(t, promoDomain.KindPercent, func(np *promoDomain.NewParams) {
		np.AppliesTo = promoDomain.AppliesToSale
	})
	repo := newFakeRepo()
	repo.put(p)
	uc := NewApplyPromotion(repo, newAppliedRepo())
	_, err := uc.Execute(context.Background(), fakeTx{}, ApplyPromotionInput{
		GymID: gymID, ActorUserID: uuid.New(),
		PromotionID: &p.ID, PaymentID: uuid.New(),
		Target: promoDomain.AppliesToMembership, Subtotal: 400, Today: time.Now(),
	}, time.Now())
	if err == nil || !errors.Is(err, promoErrors.ErrPromotionNotApplicableToTarget) {
		t.Fatalf("want target mismatch, got %v", err)
	}
}

func TestApply_CompanionRequiresMembers(t *testing.T) {
	p, gymID := newGymPromo(t, promoDomain.KindCompanionMemberships)
	repo := newFakeRepo()
	repo.put(p)
	uc := NewApplyPromotion(repo, newAppliedRepo())
	_, err := uc.Execute(context.Background(), fakeTx{}, ApplyPromotionInput{
		GymID: gymID, ActorUserID: uuid.New(),
		PromotionID: &p.ID, PaymentID: uuid.New(),
		Target: promoDomain.AppliesToMembership, Subtotal: 400, Today: time.Now(),
	}, time.Now())
	if err == nil || !errors.Is(err, promoErrors.ErrCompanionMembersRequired) {
		t.Fatalf("want companion required, got %v", err)
	}
}

func TestApply_CompanionMismatchCount(t *testing.T) {
	p, gymID := newGymPromo(t, promoDomain.KindCompanionMemberships)
	repo := newFakeRepo()
	repo.put(p)
	uc := NewApplyPromotion(repo, newAppliedRepo())
	_, err := uc.Execute(context.Background(), fakeTx{}, ApplyPromotionInput{
		GymID: gymID, ActorUserID: uuid.New(),
		PromotionID: &p.ID, PaymentID: uuid.New(),
		Target:             promoDomain.AppliesToMembership,
		Subtotal:           400,
		Today:              time.Now(),
		CompanionMemberIDs: []uuid.UUID{uuid.New(), uuid.New()}, // want 1
	}, time.Now())
	if err == nil || !errors.Is(err, promoErrors.ErrCompanionMembersMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
}

func TestApply_ByCodeCaseInsensitive(t *testing.T) {
	code := "VERANO2026"
	p, gymID := newGymPromo(t, promoDomain.KindPercent, func(np *promoDomain.NewParams) {
		np.Code = &code
	})
	repo := newFakeRepo()
	repo.put(p)
	uc := NewApplyPromotion(repo, newAppliedRepo())
	c := "verano2026"
	out, err := uc.Execute(context.Background(), fakeTx{}, ApplyPromotionInput{
		GymID: gymID, ActorUserID: uuid.New(),
		Code: &c, PaymentID: uuid.New(),
		Target: promoDomain.AppliesToMembership, Subtotal: 400, Today: time.Now(),
	}, time.Now())
	if err != nil {
		t.Fatalf("apply by code: %v", err)
	}
	if out.Discount != 100 {
		t.Errorf("want 100, got %v", out.Discount)
	}
}
