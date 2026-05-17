// Package app holds the subscription-management use cases. The cloud
// receives webhook events from Stripe / Mercado Pago, normalises them, and
// applies the resulting state change to the gym aggregate.
package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RecordEventInput is the provider-agnostic shape the webhook handlers map
// into. The handler is responsible for verifying the signature *before*
// calling here — this layer trusts the input.
type RecordEventInput struct {
	GymID        uuid.UUID
	Provider     subDomain.Provider
	Type         subDomain.EventType
	ExternalID   string
	Plan         string // "standard_monthly" / "standard_annual" / "plus_monthly" / "plus_annual" — required for activated/renewed
	Amount       *float64
	Currency     *string
	PeriodEndsAt *time.Time
	RawPayload   map[string]any
	OccurredAt   time.Time
}

// RecordEventOutput is what the controller sends back to the processor.
// `Applied` distinguishes "we accepted and updated state" from "we already had
// this event" (idempotent replay → 200 OK with applied=false).
type RecordEventOutput struct {
	EventID uuid.UUID
	Applied bool
}

type RecordEvent struct {
	Events  subDomain.EventRepository
	Gyms    gymRepo.GymRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
	NowFunc func() time.Time
}

func NewRecordEvent(events subDomain.EventRepository, gyms gymRepo.GymRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *RecordEvent {
	return &RecordEvent{
		Events:  events,
		Gyms:    gyms,
		UoW:     uow,
		Audit:   recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

func (uc *RecordEvent) Execute(ctx context.Context, in RecordEventInput) (RecordEventOutput, error) {
	if in.GymID == uuid.Nil {
		return RecordEventOutput{}, sharedDomain.NewValidationError(errors.New("gym_id is required"))
	}
	if in.ExternalID == "" {
		return RecordEventOutput{}, sharedDomain.NewValidationError(errors.New("external_id is required"))
	}
	if in.Provider == "" {
		return RecordEventOutput{}, sharedDomain.NewValidationError(errors.New("provider is required"))
	}
	if in.OccurredAt.IsZero() {
		in.OccurredAt = uc.NowFunc()
	}

	out := RecordEventOutput{}
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		exists, err := uc.Events.ExistsByExternalID(tx, in.Provider, in.ExternalID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if exists {
			out.Applied = false
			return nil
		}

		gym, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}
		now := uc.NowFunc()
		switch in.Type {
		case subDomain.EventActivated, subDomain.EventRenewed:
			plan := in.Plan
			if plan == "" {
				plan = gym.SubscriptionPlan
			}
			if plan == gymDomain.PlanTrial {
				return sharedDomain.NewValidationError(errors.New("activated event requires a paid plan"))
			}
			if err := gym.ActivateSubscription(plan, in.PeriodEndsAt, now); err != nil {
				return err
			}
		case subDomain.EventPastDue:
			gym.MarkPastDue(now)
		case subDomain.EventCancelled:
			gym.Cancel(now)
		case subDomain.EventTrialExtended:
			// Plan field doubles as days-to-extend (string-typed because the
			// canonical Plan slot here would be empty). Webhook never emits
			// this; admin tool does.
			days := 0
			if v, ok := in.RawPayload["days"]; ok {
				if n, ok := v.(float64); ok {
					days = int(n)
				}
			}
			gym.ExtendTrial(days, now)
		default:
			return sharedDomain.NewValidationError(errors.New("unknown event type"))
		}
		if _, err := uc.Gyms.Update(tx, gym); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		eventID := uuid.New()
		ev := &subDomain.Event{
			ID:           eventID,
			GymID:        in.GymID,
			Provider:     in.Provider,
			Type:         in.Type,
			ExternalID:   in.ExternalID,
			Plan:         gym.SubscriptionPlan,
			Amount:       in.Amount,
			Currency:     in.Currency,
			PeriodEndsAt: in.PeriodEndsAt,
			RawPayload:   in.RawPayload,
			OccurredAt:   in.OccurredAt,
			RecordedAt:   now,
		}
		if err := uc.Events.Insert(tx, ev); err != nil {
			if errors.Is(err, subDomain.ErrDuplicateEvent) {
				// Lost the race against another concurrent webhook. Idempotent.
				out.Applied = false
				return nil
			}
			return sharedDomain.NewUnexpectedError(err)
		}
		out.EventID = eventID
		out.Applied = true

		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:      in.GymID,
			EntityType: "gyms",
			EntityID:   in.GymID,
			Action:     "subscription_" + string(in.Type),
			Changes: map[string]any{
				"provider":    in.Provider,
				"external_id": in.ExternalID,
				"plan":        gym.SubscriptionPlan,
				"status":      gym.SubscriptionStatus,
			},
			At: now,
		})
		return nil
	})
	if err != nil {
		return RecordEventOutput{}, err
	}
	return out, nil
}
