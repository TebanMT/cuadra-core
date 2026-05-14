package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/challenges/app"
	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ─── fakes (no DB) ─────────────────────────────────────────────────────────

type fakeChallengeRepo struct {
	createdChallenge *challengeDomain.Challenge
	createErr        error
}

func (r *fakeChallengeRepo) Create(_ sharedDomain.Transaction, c *challengeDomain.Challenge) (*challengeDomain.Challenge, error) {
	if r.createErr != nil {
		return nil, r.createErr
	}
	r.createdChallenge = c
	return c, nil
}
func (r *fakeChallengeRepo) GetByID(sharedDomain.Transaction, uuid.UUID) (*challengeDomain.Challenge, error) {
	return nil, nil
}
func (r *fakeChallengeRepo) Update(_ sharedDomain.Transaction, c *challengeDomain.Challenge) (*challengeDomain.Challenge, error) {
	return c, nil
}
func (r *fakeChallengeRepo) ListByGym(sharedDomain.Transaction, uuid.UUID, string) ([]*challengeDomain.Challenge, error) {
	return nil, nil
}

type fakeTx struct{}
type fakeUoW struct{}

func (fakeTx) Execute(fn func(sharedDomain.Transaction) error) error { return fn(fakeTx{}) }
func (fakeUoW) Begin(context.Context) (sharedDomain.Transaction, error) {
	return fakeTx{}, nil
}
func (fakeUoW) Commit(sharedDomain.Transaction) error   { return nil }
func (fakeUoW) Rollback(sharedDomain.Transaction) error { return nil }
func (fakeUoW) Query(context.Context) (sharedDomain.Transaction, error) {
	return fakeTx{}, nil
}
func (fakeUoW) Command(_ context.Context, fn func(sharedDomain.Transaction) error) error {
	return fn(fakeTx{})
}

type fakeAudit struct{ entries []audit.Entry }

func (a *fakeAudit) Record(_ context.Context, _ sharedDomain.Transaction, e audit.Entry) error {
	a.entries = append(a.entries, e)
	return nil
}

// ─── tests ─────────────────────────────────────────────────────────────────

func validInput(gymID, userID uuid.UUID) app.CreateChallengeInput {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	return app.CreateChallengeInput{
		GymID:                 gymID,
		ActorUserID:           userID,
		Name:                  "Reto 12 — Edición 1",
		Description:           "Primera edición semestral",
		StartsAt:              base.Add(7 * 24 * time.Hour),
		MeasurementT0Deadline: base.Add(21 * 24 * time.Hour),
		MeasurementT1Start:    base.Add(91 * 24 * time.Hour),
		EndsAt:                base.Add(105 * 24 * time.Hour),
	}
}

func TestCreateChallenge_HappyPath(t *testing.T) {
	repo := &fakeChallengeRepo{}
	auditRec := &fakeAudit{}
	uc := app.NewCreateChallenge(repo, fakeUoW{}, auditRec)
	gymID, userID := uuid.New(), uuid.New()

	c, err := uc.Execute(context.Background(), validInput(gymID, userID))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if c.ID == uuid.Nil {
		t.Errorf("ID should be set")
	}
	if c.Status != challengeDomain.StatusDraft {
		t.Errorf("Status = %q, want draft", c.Status)
	}
	if c.GymID != gymID {
		t.Errorf("GymID mismatch")
	}
	if c.Version != 1 {
		t.Errorf("Version = %d, want 1", c.Version)
	}
	if c.StrengthCapPct != challengeDomain.DefaultStrengthCapPct {
		t.Errorf("StrengthCapPct = %v, want default", c.StrengthCapPct)
	}
	if repo.createdChallenge == nil {
		t.Errorf("Create was not called on repo")
	}
	if len(auditRec.entries) != 1 || auditRec.entries[0].Action != audit.ActionCreate {
		t.Errorf("expected one audit entry with action=create, got %+v", auditRec.entries)
	}
}

func TestCreateChallenge_AppliesOverrides(t *testing.T) {
	repo := &fakeChallengeRepo{}
	uc := app.NewCreateChallenge(repo, fakeUoW{}, &fakeAudit{})
	gymID, userID := uuid.New(), uuid.New()

	in := validInput(gymID, userID)
	customCap := 30.0
	customMargin := 7.5
	customFee := 50000 // 500 MXN
	in.StrengthCapPct = &customCap
	in.TieMarginIR = &customMargin
	in.InscriptionFeeCents = &customFee

	c, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if c.StrengthCapPct != customCap {
		t.Errorf("StrengthCapPct = %v, want %v", c.StrengthCapPct, customCap)
	}
	if c.TieMarginIR != customMargin {
		t.Errorf("TieMarginIR = %v, want %v", c.TieMarginIR, customMargin)
	}
	if c.InscriptionFeeCents != customFee {
		t.Errorf("InscriptionFeeCents = %v, want %v", c.InscriptionFeeCents, customFee)
	}
	// Critical: version must be 1 on persistence even after ApplyConfig
	// internally bumped it. Otherwise sync clients would skip v1 entirely.
	if c.Version != 1 {
		t.Errorf("Version = %d, want 1 (override path must reset)", c.Version)
	}
}

func TestCreateChallenge_RejectsEmptyName(t *testing.T) {
	repo := &fakeChallengeRepo{}
	uc := app.NewCreateChallenge(repo, fakeUoW{}, &fakeAudit{})
	in := validInput(uuid.New(), uuid.New())
	in.Name = "   "

	_, err := uc.Execute(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error for empty name")
	}
	if !errors.Is(err, challengeErrors.ErrNameRequired) {
		// Use cases wrap in ValidationError, but the sentinel should
		// still be reachable via errors.Is for handler mapping.
		t.Errorf("error = %v, want ErrNameRequired in chain", err)
	}
}

func TestCreateChallenge_RejectsBadDates(t *testing.T) {
	repo := &fakeChallengeRepo{}
	uc := app.NewCreateChallenge(repo, fakeUoW{}, &fakeAudit{})
	in := validInput(uuid.New(), uuid.New())
	// Flip T0 deadline before start
	in.MeasurementT0Deadline = in.StartsAt.Add(-1 * time.Hour)

	_, err := uc.Execute(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error for bad dates")
	}
	if !errors.Is(err, challengeErrors.ErrInvalidDates) {
		t.Errorf("error = %v, want ErrInvalidDates in chain", err)
	}
}

func TestCreateChallenge_RejectsInvalidOverrides(t *testing.T) {
	repo := &fakeChallengeRepo{}
	uc := app.NewCreateChallenge(repo, fakeUoW{}, &fakeAudit{})
	in := validInput(uuid.New(), uuid.New())
	negFee := -100
	in.InscriptionFeeCents = &negFee

	_, err := uc.Execute(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error for negative fee")
	}
}
