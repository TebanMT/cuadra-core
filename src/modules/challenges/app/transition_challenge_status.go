package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	challengeRepo "github.com/cuadra/cuadra-core/src/modules/challenges/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Transition keywords accepted on the wire. Kept as small constants here
// (rather than reusing the StatusXxx strings) because the keyword names
// the *action*, not the resulting status — easier to reason about for
// the FE button copy.
const (
	TransitionOpenRegistration = "open_registration"
	TransitionStartRunning     = "start_running"
	TransitionStartMeasuringT1 = "start_measuring_t1"
	TransitionClose            = "close"
	TransitionCancel           = "cancel"
)

// TransitionChallengeStatus is UC-Reto-003. Routes to the right state
// machine method on the Challenge entity. The "≥1 category" precondition
// for OpenRegistration lives here because the entity itself can't see the
// category repository.
type TransitionChallengeStatus struct {
	Challenges challengeRepo.ChallengeRepository
	Categories challengeRepo.CategoryRepository
	UoW        sharedDomain.UnitOfWork
	Audit      audit.Recorder
	NowFunc    func() time.Time
}

func NewTransitionChallengeStatus(
	challenges challengeRepo.ChallengeRepository,
	categories challengeRepo.CategoryRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *TransitionChallengeStatus {
	return &TransitionChallengeStatus{
		Challenges: challenges, Categories: categories,
		UoW: uow, Audit: recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type TransitionChallengeStatusInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	ChallengeID uuid.UUID
	Transition  string
}

func (uc *TransitionChallengeStatus) Execute(ctx context.Context, in TransitionChallengeStatusInput) (*challengeDomain.Challenge, error) {
	now := uc.NowFunc()
	var result *challengeDomain.Challenge
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		c, err := uc.Challenges.GetByID(tx, in.ChallengeID)
		if err != nil {
			return err
		}
		if c.GymID != in.GymID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		prevStatus := c.Status

		switch in.Transition {
		case TransitionOpenRegistration:
			// At-least-one-category precondition lives at the use-case
			// layer (entity can't see the repo).
			cats, err := uc.Categories.ListByChallenge(tx, c.ID)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if len(cats) == 0 {
				return sharedDomain.NewBusinessError(challengeErrors.ErrNoCategories, "")
			}
			if err := c.OpenRegistration(now); err != nil {
				return sharedDomain.NewBusinessError(err, "")
			}
		case TransitionStartRunning:
			if err := c.StartRunning(now); err != nil {
				return sharedDomain.NewBusinessError(err, "")
			}
		case TransitionStartMeasuringT1:
			if err := c.StartMeasuringT1(now); err != nil {
				return sharedDomain.NewBusinessError(err, "")
			}
		case TransitionClose:
			if err := c.Close(now); err != nil {
				return sharedDomain.NewBusinessError(err, "")
			}
		case TransitionCancel:
			if err := c.Cancel(now); err != nil {
				return sharedDomain.NewBusinessError(err, "")
			}
		default:
			return sharedDomain.NewValidationError(challengeErrors.ErrInvalidStatusTransition)
		}

		updated, err := uc.Challenges.Update(tx, c)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		result = updated
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "challenges",
			EntityID:    c.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"from": prevStatus,
				"to":   c.Status,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
