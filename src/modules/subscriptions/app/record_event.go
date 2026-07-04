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
	// StripeCustomerID — capturado del webhook Stripe (customer.subscription.*,
	// invoice.*, checkout.session.completed). El use case lo asigna al gym
	// SÓLO si está vacío; no pisamos un id existente (defense contra
	// re-key accidental por cambio manual en Stripe dashboard).
	StripeCustomerID string
	RawPayload       map[string]any
	OccurredAt       time.Time
}

// RecordEventOutput is what the controller sends back to the processor.
// `Applied` distinguishes "we accepted and updated state" from "we already had
// this event" (idempotent replay → 200 OK with applied=false).
type RecordEventOutput struct {
	EventID uuid.UUID
	Applied bool
}

// GymSyncToucher bumpea la fila del gym en el journal de sync del cloud
// (sync_entities.server_updated_at) para que el próximo pull INCREMENTAL de
// los sidecars la incluya. Los cambios de suscripción son cloud-authoritative
// (webhooks Stripe/MP) y escriben la tabla `gyms` sin pasar por el push
// pipeline — sin este touch, la fila nunca entra al delta y el desktop se
// queda con el status viejo hasta un full sync (bug jul-2026: dueño paga,
// cloud queda active, desktop sigue mostrando "te quedan X días de prueba").
//
// Basta bumpear el timestamp — NO se re-serializa el payload: la
// augmentation del pull (gymCanonicalAugmentExpr en shared/sync) inyecta
// subscription_plan/status/trial_ends_at/subscription_ends_at VIVOS de la
// tabla gyms al construir cada payload.
//
// Nil en el binario sidecar y en tests unitarios (el journal sólo existe en
// el cloud) — mismo patrón que ProcessWebhook.OptOut.
type GymSyncToucher interface {
	TouchGym(ctx context.Context, tx sharedDomain.Transaction, gymID uuid.UUID) error
}

type RecordEvent struct {
	Events  subDomain.EventRepository
	Gyms    gymRepo.GymRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
	NowFunc func() time.Time
	// SyncTouch (opcional; se cablea sólo en cmd/server). Ver GymSyncToucher.
	SyncTouch GymSyncToucher
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

		// Reject out-of-order webhooks. Stripe/MP retry on non-2xx and can
		// re-deliver older events after a newer state transition; the
		// (provider, external_id) idempotency only catches exact duplicates,
		// not the case where a retry of an older `invoice.paid` arrives
		// AFTER a `customer.subscription.deleted` was applied. Compare
		// against the max occurred_at previously applied to this gym and
		// drop anything strictly older. We allow equality so that ties at
		// the same wall-clock instant still apply (rare but possible).
		latest, err := uc.Events.LatestOccurredAtForGym(tx, in.GymID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if latest != nil && in.OccurredAt.Before(*latest) {
			out.Applied = false
			return nil
		}

		// subscriptionChanged: el evento tocó el estado de suscripción del
		// gym (status / plan / fechas) → hay que re-mandar la fila del gym
		// a los sidecars vía el touch de sync_entities. Los eventos
		// voucher_* NO cambian nada del gym a propósito y no bumpean.
		subscriptionChanged := false
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
			subscriptionChanged = true
		case subDomain.EventPastDue:
			gym.MarkPastDue(now)
			subscriptionChanged = true
		case subDomain.EventCancelled:
			gym.Cancel(now)
			subscriptionChanged = true
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
			subscriptionChanged = true
		case subDomain.EventVoucherEmitted, subDomain.EventVoucherExpired:
			// No-op sobre el gym a propósito. Estos eventos sólo dejan
			// rastro en subscription_events para que el dashboard pueda
			// derivar "tienes ficha pendiente" / "tu ficha venció".
			//   - voucher_emitted: el dueño todavía no paga. No cambiamos
			//     status — si estaba trial, sigue trial; si estaba active,
			//     sigue active hasta SubscriptionEndsAt natural.
			//   - voucher_expired: tampoco regresamos a past_due. Past_due
			//     fue diseñado para "tarjeta falló N veces"; un voucher
			//     vencido es un signal distinto (cliente nunca fue a OXXO).
			//     Si el voucher era para renovar un anual activo, el cron
			//     que vigila SubscriptionEndsAt decidirá cancelar cuando
			//     la fecha pase, no este branch.
		default:
			return sharedDomain.NewValidationError(errors.New("unknown event type"))
		}
		// Captura del customer Stripe en el primer evento que lo trae. Sólo
		// asignamos si no había uno previo — re-key manual en Stripe dashboard
		// (raro pero posible) no debe romper el portal del gym actual.
		if in.StripeCustomerID != "" && (gym.StripeCustomerID == nil || *gym.StripeCustomerID == "") {
			id := in.StripeCustomerID
			gym.StripeCustomerID = &id
		}
		if _, err := uc.Gyms.Update(tx, gym); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// Mismo tx que el Update: si el touch falla, todo rollbackea y el
		// provider reintenta el webhook — nunca un gym activo en la tabla
		// con un journal que no lo propaga.
		if subscriptionChanged && uc.SyncTouch != nil {
			if err := uc.SyncTouch.TouchGym(ctx, tx, gym.ID); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
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
