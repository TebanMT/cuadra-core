package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/challenges/app"
	categoryDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/category"
	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// inMemoryMeasurementRepo holds rows in a slice and implements supersession
// in-place. Faithful enough to test the use case's ordering invariant
// without involving SQLite.
type inMemoryMeasurementRepo struct {
	rows []*measurementDomain.Measurement
}

func (r *inMemoryMeasurementRepo) Create(_ sharedDomain.Transaction, m *measurementDomain.Measurement) (*measurementDomain.Measurement, error) {
	r.rows = append(r.rows, m)
	return m, nil
}
func (r *inMemoryMeasurementRepo) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*measurementDomain.Measurement, error) {
	for _, m := range r.rows {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, sharedDomain.NewBusinessError(challengeErrors.ErrMeasurementNotFound, "")
}
func (r *inMemoryMeasurementRepo) Supersede(_ sharedDomain.Transaction, priorID, replacementID uuid.UUID, at time.Time) error {
	for _, m := range r.rows {
		if m.ID == priorID && m.SupersededAt == nil {
			m.MarkSuperseded(replacementID, at)
			return nil
		}
	}
	return nil
}
func (r *inMemoryMeasurementRepo) ListByParticipant(_ sharedDomain.Transaction, participantID uuid.UUID) ([]*measurementDomain.Measurement, error) {
	var out []*measurementDomain.Measurement
	for _, m := range r.rows {
		if m.ParticipantID == participantID {
			out = append(out, m)
		}
	}
	return out, nil
}
func (r *inMemoryMeasurementRepo) GetActiveByMoment(_ sharedDomain.Transaction, participantID uuid.UUID, moment string) (*measurementDomain.Measurement, bool, error) {
	for _, m := range r.rows {
		if m.ParticipantID == participantID && m.Moment == moment && m.SupersededAt == nil && m.DeletedAt == nil {
			return m, true, nil
		}
	}
	return nil, false, nil
}
func (r *inMemoryMeasurementRepo) CountByChallenge(sharedDomain.Transaction, uuid.UUID) (int, error) {
	return len(r.rows), nil
}

// validMeasurementInput returns a passing input for the captures used here.
func validMeasurementInput(now time.Time, weight float64) measurementDomain.Input {
	return measurementDomain.Input{
		Moment:          measurementDomain.MomentT0,
		MeasuredAt:      now,
		BodyWeightKg:    weight,
		BodyFatPct:      22,
		LegsWeightKg:    80,
		LegsReps:        5,
		PushWeightKg:    60,
		PushReps:        5,
		PullWeightKg:    100,
		PullReps:        3,
		CreatedByUserID: uuid.New(),
	}
}

func TestCaptureMeasurement_SupersedesPriorMeasurement(t *testing.T) {
	gymID := uuid.New()
	ch := challengeInStatus(t, gymID, challengeDomain.StatusOpenRegistration)
	now := ch.StartsAt.Add(24 * time.Hour) // inside T₀ window
	cat, _ := categoryDomain.NewCategory(uuid.New(), gymID, ch.ID, "Hombres", 0, now)
	p := participantDomain.NewParticipant(uuid.New(), gymID, ch.ID, uuid.New(), cat.ID, participantDomain.ExerciseSelection{}, now)

	chRepo := &stubChRepo{stored: ch}
	partRepo := newFakeParticipantRepo()
	partRepo.rows[p.ID] = p
	_ = newFakeCategoryRepo(cat) // not needed here
	mRepo := &inMemoryMeasurementRepo{}

	uc := app.NewCaptureMeasurement(chRepo, partRepo, mRepo, fakeUoW{}, &fakeAudit{})
	uc.NowFunc = func() time.Time { return now }

	// First T₀ — establishes the baseline.
	first, err := uc.Execute(context.Background(), app.CaptureMeasurementInput{
		GymID: gymID, ActorUserID: uuid.New(),
		ChallengeID: ch.ID, ParticipantID: p.ID,
		Input: validMeasurementInput(now, 80),
	})
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	if first.SupersededPriorID != nil {
		t.Errorf("first capture should not supersede anything, got %v", first.SupersededPriorID)
	}

	// Operator notices a typo and re-captures T₀ — should supersede the first.
	second, err := uc.Execute(context.Background(), app.CaptureMeasurementInput{
		GymID: gymID, ActorUserID: uuid.New(),
		ChallengeID: ch.ID, ParticipantID: p.ID,
		Input: validMeasurementInput(now, 81), // corrected weight
	})
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if second.SupersededPriorID == nil {
		t.Fatalf("second capture should supersede the first")
	}
	if *second.SupersededPriorID != first.Measurement.ID {
		t.Errorf("SupersededPriorID = %v, want %v", *second.SupersededPriorID, first.Measurement.ID)
	}

	// State invariant: exactly one active T₀ measurement remains.
	active, ok, err := mRepo.GetActiveByMoment(nil, p.ID, measurementDomain.MomentT0)
	if err != nil {
		t.Fatalf("GetActiveByMoment: %v", err)
	}
	if !ok {
		t.Fatalf("expected an active T₀ after supersession")
	}
	if active.ID != second.Measurement.ID {
		t.Errorf("active = %v, want second %v", active.ID, second.Measurement.ID)
	}

	// The full list still contains both rows; only one is active.
	all, _ := mRepo.ListByParticipant(nil, p.ID)
	if len(all) != 2 {
		t.Errorf("ListByParticipant returned %d rows, want 2", len(all))
	}
	activeCount := 0
	for _, m := range all {
		if m.IsActive() {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Errorf("active rows = %d, want 1", activeCount)
	}
}

func TestCaptureMeasurement_T0PromotesRegisteredToActive(t *testing.T) {
	gymID := uuid.New()
	ch := challengeInStatus(t, gymID, challengeDomain.StatusOpenRegistration)
	now := ch.StartsAt.Add(24 * time.Hour)
	cat, _ := categoryDomain.NewCategory(uuid.New(), gymID, ch.ID, "Hombres", 0, now)
	p := participantDomain.NewParticipant(uuid.New(), gymID, ch.ID, uuid.New(), cat.ID, participantDomain.ExerciseSelection{}, now)
	if p.Status != participantDomain.StatusRegistered {
		t.Fatalf("fixture should start as registered, got %q", p.Status)
	}

	chRepo := &stubChRepo{stored: ch}
	partRepo := newFakeParticipantRepo()
	partRepo.rows[p.ID] = p
	mRepo := &inMemoryMeasurementRepo{}

	uc := app.NewCaptureMeasurement(chRepo, partRepo, mRepo, fakeUoW{}, &fakeAudit{})
	uc.NowFunc = func() time.Time { return now }

	out, err := uc.Execute(context.Background(), app.CaptureMeasurementInput{
		GymID: gymID, ActorUserID: uuid.New(),
		ChallengeID: ch.ID, ParticipantID: p.ID,
		Input: validMeasurementInput(now, 80),
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if out.ParticipantStatus != participantDomain.StatusActive {
		t.Errorf("ParticipantStatus = %q, want active after T₀", out.ParticipantStatus)
	}
}
