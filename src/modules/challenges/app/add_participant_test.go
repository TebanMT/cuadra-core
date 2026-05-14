package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/challenges/app"
	categoryDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/category"
	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// fakeCategoryRepo + fakeParticipantRepo are the in-memory stand-ins. They
// track *what* the use case wrote so the assertions can check business
// invariants without depending on SQL.

type fakeCategoryRepo struct{ rows map[uuid.UUID]*categoryDomain.Category }

func newFakeCategoryRepo(cats ...*categoryDomain.Category) *fakeCategoryRepo {
	r := &fakeCategoryRepo{rows: map[uuid.UUID]*categoryDomain.Category{}}
	for _, c := range cats {
		r.rows[c.ID] = c
	}
	return r
}

func (r *fakeCategoryRepo) Create(_ sharedDomain.Transaction, c *categoryDomain.Category) (*categoryDomain.Category, error) {
	r.rows[c.ID] = c
	return c, nil
}
func (r *fakeCategoryRepo) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*categoryDomain.Category, error) {
	if c, ok := r.rows[id]; ok {
		return c, nil
	}
	return nil, sharedDomain.NewBusinessError(challengeErrors.ErrCategoryNotFound, "")
}
func (r *fakeCategoryRepo) Update(_ sharedDomain.Transaction, c *categoryDomain.Category) (*categoryDomain.Category, error) {
	r.rows[c.ID] = c
	return c, nil
}
func (r *fakeCategoryRepo) SoftDelete(_ sharedDomain.Transaction, id uuid.UUID) error {
	delete(r.rows, id)
	return nil
}
func (r *fakeCategoryRepo) ListByChallenge(_ sharedDomain.Transaction, challengeID uuid.UUID) ([]*categoryDomain.Category, error) {
	var out []*categoryDomain.Category
	for _, c := range r.rows {
		if c.ChallengeID == challengeID && c.DeletedAt == nil {
			out = append(out, c)
		}
	}
	return out, nil
}
func (r *fakeCategoryRepo) CountParticipants(sharedDomain.Transaction, uuid.UUID) (int, error) {
	return 0, nil
}

type fakeParticipantRepo struct {
	rows           map[uuid.UUID]*participantDomain.Participant
	hasMeasurement bool
}

func newFakeParticipantRepo() *fakeParticipantRepo {
	return &fakeParticipantRepo{rows: map[uuid.UUID]*participantDomain.Participant{}}
}

func (r *fakeParticipantRepo) Create(_ sharedDomain.Transaction, p *participantDomain.Participant) (*participantDomain.Participant, error) {
	r.rows[p.ID] = p
	return p, nil
}
func (r *fakeParticipantRepo) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*participantDomain.Participant, error) {
	if p, ok := r.rows[id]; ok {
		return p, nil
	}
	return nil, sharedDomain.NewBusinessError(challengeErrors.ErrParticipantNotFound, "")
}
func (r *fakeParticipantRepo) Update(_ sharedDomain.Transaction, p *participantDomain.Participant) (*participantDomain.Participant, error) {
	r.rows[p.ID] = p
	return p, nil
}
func (r *fakeParticipantRepo) SoftDelete(_ sharedDomain.Transaction, id uuid.UUID) error {
	delete(r.rows, id)
	return nil
}
func (r *fakeParticipantRepo) ListByChallenge(_ sharedDomain.Transaction, challengeID uuid.UUID, statusFilter string, categoryFilter *uuid.UUID) ([]*participantDomain.Participant, error) {
	var out []*participantDomain.Participant
	for _, p := range r.rows {
		if p.ChallengeID != challengeID || p.DeletedAt != nil {
			continue
		}
		if statusFilter != "" && p.Status != statusFilter {
			continue
		}
		if categoryFilter != nil && p.CategoryID != *categoryFilter {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}
func (r *fakeParticipantRepo) ExistsByMember(_ sharedDomain.Transaction, challengeID, memberID uuid.UUID) (bool, error) {
	for _, p := range r.rows {
		if p.ChallengeID == challengeID && p.MemberID == memberID && p.DeletedAt == nil {
			return true, nil
		}
	}
	return false, nil
}
func (r *fakeParticipantRepo) HasAnyMeasurement(sharedDomain.Transaction, uuid.UUID) (bool, error) {
	return r.hasMeasurement, nil
}

// challengeAt sets up a challenge in the given status by walking the state
// machine — simpler than reaching into private fields.
func challengeInStatus(t *testing.T, gymID uuid.UUID, status string) *challengeDomain.Challenge {
	t.Helper()
	c := makeChallengeFixture(gymID)
	now := c.CreatedAt
	if status == challengeDomain.StatusDraft {
		return c
	}
	if err := c.OpenRegistration(now); err != nil {
		t.Fatalf("OpenRegistration: %v", err)
	}
	if status == challengeDomain.StatusOpenRegistration {
		return c
	}
	if err := c.StartRunning(now); err != nil {
		t.Fatalf("StartRunning: %v", err)
	}
	return c
}

func TestAddParticipant_RejectsWhenChallengeNotRegistering(t *testing.T) {
	gymID := uuid.New()
	ch := challengeInStatus(t, gymID, challengeDomain.StatusDraft) // still draft → not accepting
	chRepo := &stubChRepo{stored: ch}
	now := time.Now().UTC()
	cat, _ := categoryDomain.NewCategory(uuid.New(), gymID, ch.ID, "Hombres", 0, now)
	catRepo := newFakeCategoryRepo(cat)
	partRepo := newFakeParticipantRepo()

	uc := app.NewAddParticipant(chRepo, catRepo, partRepo, fakeUoW{}, &fakeAudit{})
	_, err := uc.Execute(context.Background(), app.AddParticipantInput{
		GymID: gymID, ActorUserID: uuid.New(),
		ChallengeID: ch.ID, MemberID: uuid.New(), CategoryID: cat.ID,
	})
	if err == nil {
		t.Fatalf("expected ErrChallengeNotRegistering")
	}
	if !errors.Is(err, challengeErrors.ErrChallengeNotRegistering) {
		t.Errorf("error = %v, want ErrChallengeNotRegistering in chain", err)
	}
}

func TestAddParticipant_RejectsCategoryFromAnotherChallenge(t *testing.T) {
	gymID := uuid.New()
	ch := challengeInStatus(t, gymID, challengeDomain.StatusOpenRegistration)
	now := ch.CreatedAt
	otherChallengeID := uuid.New()
	cat, _ := categoryDomain.NewCategory(uuid.New(), gymID, otherChallengeID, "Hombres", 0, now)

	chRepo := &stubChRepo{stored: ch}
	catRepo := newFakeCategoryRepo(cat)
	partRepo := newFakeParticipantRepo()

	uc := app.NewAddParticipant(chRepo, catRepo, partRepo, fakeUoW{}, &fakeAudit{})
	_, err := uc.Execute(context.Background(), app.AddParticipantInput{
		GymID: gymID, ActorUserID: uuid.New(),
		ChallengeID: ch.ID, MemberID: uuid.New(), CategoryID: cat.ID,
	})
	if err == nil {
		t.Fatalf("expected ErrCategoryMismatch")
	}
	if !errors.Is(err, challengeErrors.ErrCategoryMismatch) {
		t.Errorf("error = %v, want ErrCategoryMismatch in chain", err)
	}
}

func TestAddParticipant_RejectsAlreadyInscribed(t *testing.T) {
	gymID := uuid.New()
	ch := challengeInStatus(t, gymID, challengeDomain.StatusOpenRegistration)
	now := ch.CreatedAt
	cat, _ := categoryDomain.NewCategory(uuid.New(), gymID, ch.ID, "Hombres", 0, now)

	memberID := uuid.New()
	chRepo := &stubChRepo{stored: ch}
	catRepo := newFakeCategoryRepo(cat)
	partRepo := newFakeParticipantRepo()
	// Seed an existing participant for the same member.
	partRepo.rows[uuid.New()] = participantDomain.NewParticipant(
		uuid.New(), gymID, ch.ID, memberID, cat.ID,
		participantDomain.ExerciseSelection{}, now,
	)

	uc := app.NewAddParticipant(chRepo, catRepo, partRepo, fakeUoW{}, &fakeAudit{})
	_, err := uc.Execute(context.Background(), app.AddParticipantInput{
		GymID: gymID, ActorUserID: uuid.New(),
		ChallengeID: ch.ID, MemberID: memberID, CategoryID: cat.ID,
	})
	if err == nil {
		t.Fatalf("expected ErrAlreadyParticipating")
	}
	if !errors.Is(err, challengeErrors.ErrAlreadyParticipating) {
		t.Errorf("error = %v, want ErrAlreadyParticipating in chain", err)
	}
}

func TestAddParticipant_HappyPath_DefaultsExercises(t *testing.T) {
	gymID := uuid.New()
	ch := challengeInStatus(t, gymID, challengeDomain.StatusOpenRegistration)
	now := ch.CreatedAt
	cat, _ := categoryDomain.NewCategory(uuid.New(), gymID, ch.ID, "Hombres", 0, now)

	chRepo := &stubChRepo{stored: ch}
	catRepo := newFakeCategoryRepo(cat)
	partRepo := newFakeParticipantRepo()

	uc := app.NewAddParticipant(chRepo, catRepo, partRepo, fakeUoW{}, &fakeAudit{})
	p, err := uc.Execute(context.Background(), app.AddParticipantInput{
		GymID: gymID, ActorUserID: uuid.New(),
		ChallengeID: ch.ID, MemberID: uuid.New(), CategoryID: cat.ID,
		// Exercises left blank — defaults should fill them in.
	})
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if p.ExerciseLegs != participantDomain.ExerciseLegsSquat {
		t.Errorf("ExerciseLegs = %q, want %q", p.ExerciseLegs, participantDomain.ExerciseLegsSquat)
	}
	if p.ExercisePush != participantDomain.ExercisePushBenchPress {
		t.Errorf("ExercisePush = %q, want %q", p.ExercisePush, participantDomain.ExercisePushBenchPress)
	}
	if p.ExercisePull != participantDomain.ExercisePullDeadlift {
		t.Errorf("ExercisePull = %q, want %q", p.ExercisePull, participantDomain.ExercisePullDeadlift)
	}
}
