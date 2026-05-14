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
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// fakeMeasurementRepo is a minimal stand-in so UpdateChallengeConfig can ask
// "any measurement captured?". Implement only what the use case calls;
// the rest can panic (they're never reached in this test).
type fakeMeasurementRepo struct{ count int }

func (r *fakeMeasurementRepo) Create(sharedDomain.Transaction, *measurementDomain.Measurement) (*measurementDomain.Measurement, error) {
	panic("unused")
}
func (r *fakeMeasurementRepo) GetByID(sharedDomain.Transaction, uuid.UUID) (*measurementDomain.Measurement, error) {
	panic("unused")
}
func (r *fakeMeasurementRepo) Supersede(sharedDomain.Transaction, uuid.UUID, uuid.UUID, time.Time) error {
	panic("unused")
}
func (r *fakeMeasurementRepo) ListByParticipant(sharedDomain.Transaction, uuid.UUID) ([]*measurementDomain.Measurement, error) {
	panic("unused")
}
func (r *fakeMeasurementRepo) GetActiveByMoment(sharedDomain.Transaction, uuid.UUID, string) (*measurementDomain.Measurement, bool, error) {
	panic("unused")
}
func (r *fakeMeasurementRepo) CountByChallenge(sharedDomain.Transaction, uuid.UUID) (int, error) {
	return r.count, nil
}

func makeChallengeFixture(gymID uuid.UUID) *challengeDomain.Challenge {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c, err := challengeDomain.NewChallenge(
		uuid.New(), gymID,
		"Reto 12", "",
		base.Add(7*24*time.Hour),
		base.Add(21*24*time.Hour),
		base.Add(91*24*time.Hour),
		base.Add(105*24*time.Hour),
		base,
	)
	if err != nil {
		panic(err)
	}
	return c
}

func TestUpdateChallengeConfig_RejectsOnceMeasurementsExist(t *testing.T) {
	gymID := uuid.New()
	stored := makeChallengeFixture(gymID)

	chRepo := &fakeChallengeRepo{}
	chRepo.createdChallenge = stored
	// Stub GetByID via closure on a wrapper. The shared fakeChallengeRepo from
	// create_challenge_test.go's GetByID returns nil — we need it to return stored.
	getter := &stubChRepo{stored: stored}
	mRepo := &fakeMeasurementRepo{count: 1} // one measurement already captured

	uc := app.NewUpdateChallengeConfig(getter, mRepo, fakeUoW{}, &fakeAudit{})
	cap := 30.0
	_, err := uc.Execute(context.Background(), app.UpdateChallengeConfigInput{
		GymID:       gymID,
		ActorUserID: uuid.New(),
		ChallengeID: stored.ID,
		Config:      challengeDomain.ConfigUpdate{StrengthCapPct: &cap},
	})
	if err == nil {
		t.Fatalf("expected ErrConfigLocked when measurements exist")
	}
	if !errors.Is(err, challengeErrors.ErrConfigLocked) {
		t.Errorf("error = %v, want ErrConfigLocked in chain", err)
	}
}

func TestUpdateChallengeConfig_HappyPathBeforeMeasurements(t *testing.T) {
	gymID := uuid.New()
	stored := makeChallengeFixture(gymID)
	mRepo := &fakeMeasurementRepo{count: 0}
	getter := &stubChRepo{stored: stored}

	uc := app.NewUpdateChallengeConfig(getter, mRepo, fakeUoW{}, &fakeAudit{})
	cap := 30.0
	out, err := uc.Execute(context.Background(), app.UpdateChallengeConfigInput{
		GymID: gymID, ActorUserID: uuid.New(), ChallengeID: stored.ID,
		Config: challengeDomain.ConfigUpdate{StrengthCapPct: &cap},
	})
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if out.StrengthCapPct != cap {
		t.Errorf("StrengthCapPct = %v, want %v", out.StrengthCapPct, cap)
	}
}

func TestUpdateChallengeConfig_RejectsCrossGym(t *testing.T) {
	stored := makeChallengeFixture(uuid.New())
	mRepo := &fakeMeasurementRepo{count: 0}
	getter := &stubChRepo{stored: stored}

	uc := app.NewUpdateChallengeConfig(getter, mRepo, fakeUoW{}, &fakeAudit{})
	cap := 30.0
	_, err := uc.Execute(context.Background(), app.UpdateChallengeConfigInput{
		GymID:       uuid.New(), // different gym than the stored challenge
		ActorUserID: uuid.New(),
		ChallengeID: stored.ID,
		Config:      challengeDomain.ConfigUpdate{StrengthCapPct: &cap},
	})
	if err == nil {
		t.Fatalf("expected ErrCrossGym")
	}
	if !errors.Is(err, challengeErrors.ErrCrossGym) {
		t.Errorf("error = %v, want ErrCrossGym in chain", err)
	}
}

// stubChRepo returns a single stored challenge from GetByID. Mutations are
// not persisted — the in-memory entity itself records the version bump.
type stubChRepo struct{ stored *challengeDomain.Challenge }

func (r *stubChRepo) Create(sharedDomain.Transaction, *challengeDomain.Challenge) (*challengeDomain.Challenge, error) {
	panic("unused")
}
func (r *stubChRepo) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*challengeDomain.Challenge, error) {
	if r.stored != nil && r.stored.ID == id {
		return r.stored, nil
	}
	return nil, sharedDomain.NewBusinessError(challengeErrors.ErrChallengeNotFound, "")
}
func (r *stubChRepo) Update(_ sharedDomain.Transaction, c *challengeDomain.Challenge) (*challengeDomain.Challenge, error) {
	r.stored = c
	return c, nil
}
func (r *stubChRepo) ListByGym(sharedDomain.Transaction, uuid.UUID, string) ([]*challengeDomain.Challenge, error) {
	panic("unused")
}
