package app

import (
	"context"
	"errors"

	"github.com/google/uuid"

	categoryDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/category"
	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	challengeRepo "github.com/cuadra/cuadra-core/src/modules/challenges/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// GetChallengeDetail returns the challenge plus light aggregates the
// detail screen needs without forcing the FE to fan-out multiple calls.
type GetChallengeDetail struct {
	Challenges   challengeRepo.ChallengeRepository
	Categories   challengeRepo.CategoryRepository
	Participants challengeRepo.ParticipantRepository
	Measurements challengeRepo.MeasurementRepository
	UoW          sharedDomain.UnitOfWork
}

func NewGetChallengeDetail(
	challenges challengeRepo.ChallengeRepository,
	categories challengeRepo.CategoryRepository,
	participants challengeRepo.ParticipantRepository,
	measurements challengeRepo.MeasurementRepository,
	uow sharedDomain.UnitOfWork,
) *GetChallengeDetail {
	return &GetChallengeDetail{
		Challenges: challenges, Categories: categories,
		Participants: participants, Measurements: measurements,
		UoW: uow,
	}
}

type GetChallengeDetailInput struct {
	GymID       uuid.UUID
	ChallengeID uuid.UUID
}

type GetChallengeDetailOutput struct {
	Challenge        *challengeDomain.Challenge
	Categories       []*categoryDomain.Category
	ParticipantCount int
	T0Captured       int
	T1Captured       int
}

func (uc *GetChallengeDetail) Execute(ctx context.Context, in GetChallengeDetailInput) (*GetChallengeDetailOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	ch, err := uc.Challenges.GetByID(tx, in.ChallengeID)
	if err != nil {
		return nil, err
	}
	if ch.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
	}
	cats, err := uc.Categories.ListByChallenge(tx, ch.ID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	parts, err := uc.Participants.ListByChallenge(tx, ch.ID, "", nil)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	// Walk every participant once and count active T₀/T₁. Cheaper than a
	// SQL aggregate because most retos top out at ~50 participants.
	var t0, t1 int
	for _, p := range parts {
		if _, ok, err := uc.Measurements.GetActiveByMoment(tx, p.ID, measurementDomain.MomentT0); err != nil && !errors.Is(err, sharedDomain.NewBusinessError(challengeErrors.ErrMeasurementNotFound, "")) {
			return nil, sharedDomain.NewUnexpectedError(err)
		} else if ok {
			t0++
		}
		if _, ok, err := uc.Measurements.GetActiveByMoment(tx, p.ID, measurementDomain.MomentT1); err != nil && !errors.Is(err, sharedDomain.NewBusinessError(challengeErrors.ErrMeasurementNotFound, "")) {
			return nil, sharedDomain.NewUnexpectedError(err)
		} else if ok {
			t1++
		}
	}
	return &GetChallengeDetailOutput{
		Challenge:        ch,
		Categories:       cats,
		ParticipantCount: len(parts),
		T0Captured:       t0,
		T1Captured:       t1,
	}, nil
}
