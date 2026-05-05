package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// GetSubscriptionOutput is the read model the FE settings page consumes. It
// flattens the gym aggregate's billing fields and tacks on a recent event
// history.
type GetSubscriptionOutput struct {
	GymID          uuid.UUID
	Plan           string
	Status         string
	TrialEndsAt    *time.Time
	PeriodEndsAt   *time.Time
	HasActiveAcc   bool
	IsTrialExpired bool
	History        []*subDomain.Event
}

type GetSubscription struct {
	Events  subDomain.EventRepository
	Gyms    gymRepo.GymRepository
	UoW     sharedDomain.UnitOfWork
	NowFunc func() time.Time
}

func NewGetSubscription(events subDomain.EventRepository, gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork) *GetSubscription {
	return &GetSubscription{
		Events:  events,
		Gyms:    gyms,
		UoW:     uow,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

func (uc *GetSubscription) Execute(ctx context.Context, gymID uuid.UUID) (*GetSubscriptionOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	gym, err := uc.Gyms.GetByID(tx, gymID)
	if err != nil {
		return nil, err
	}
	hist, err := uc.Events.ListByGym(tx, gymID, 25)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := uc.NowFunc()
	return &GetSubscriptionOutput{
		GymID:          gym.ID,
		Plan:           gym.SubscriptionPlan,
		Status:         gym.SubscriptionStatus,
		TrialEndsAt:    gym.TrialEndsAt,
		PeriodEndsAt:   gym.SubscriptionEndsAt,
		HasActiveAcc:   gym.HasActiveAccess(now),
		IsTrialExpired: gym.IsTrialExpired(now),
		History:        hist,
	}, nil
}

// Sentinel: keep gymDomain referenced (unused otherwise — go vet happiness).
var _ = gymDomain.PlanTrial
