// Package domain defines the subscription bounded context. The "state" of a
// subscription is held in the gyms aggregate (`Gym.SubscriptionStatus` +
// `SubscriptionPlan` + `SubscriptionEndsAt`). This BC owns the *transitions*
// — the use cases that translate a payment-processor event into a domain
// mutation, and the persisted audit trail (`subscription_events` table) so we
// can reconcile against Stripe/Mercado Pago after the fact.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// Provider identifies which processor a webhook came from.
type Provider string

const (
	ProviderStripe      Provider = "stripe"
	ProviderMercadoPago Provider = "mercadopago"
	ProviderManual      Provider = "manual"
)

// EventType is the canonical (provider-agnostic) verb for the transition the
// processor reported. The use case maps Stripe / MP raw events onto these.
type EventType string

const (
	// EventActivated — first paid charge or new subscription.created.
	EventActivated EventType = "activated"
	// EventRenewed — periodic invoice paid; ends_at extended.
	EventRenewed EventType = "renewed"
	// EventPastDue — payment failed; warning state.
	EventPastDue EventType = "past_due"
	// EventCancelled — subscription cancelled (immediate or end-of-period).
	EventCancelled EventType = "cancelled"
	// EventTrialExtended — manual sales action; not a processor event.
	EventTrialExtended EventType = "trial_extended"
	// EventVoucherEmitted — voucher OXXO emitido (Stripe Checkout
	// mode=payment con payment_status=unpaid). NO muta el gym; sólo deja
	// rastro en subscription_events para que el dashboard pueda mostrar
	// "tienes una ficha pendiente, pagala antes del X". Si el cliente
	// paga, llegará luego un EventActivated; si vence, un EventVoucherExpired.
	EventVoucherEmitted EventType = "voucher_emitted"
	// EventVoucherExpired — voucher OXXO venció sin pagarse. Tampoco muta
	// el gym (un voucher vencido en trial no debe acabar el trial; un
	// voucher vencido renovando un anual activo se maneja por
	// SubscriptionEndsAt + cron, no por past_due). Sólo es visibilidad.
	EventVoucherExpired EventType = "voucher_expired"
)

// Event is one row in `subscription_events`. We store the raw provider payload
// in case a future migration / dispute needs it. The cloud is the source of
// truth for billing — sidecars never see this table.
type Event struct {
	ID           uuid.UUID
	GymID        uuid.UUID
	Provider     Provider
	Type         EventType
	ExternalID   string         // Stripe event id / MP notification id (idempotency)
	Plan         string         // resulting Gym.SubscriptionPlan
	Amount       *float64       // optional, what was charged
	Currency     *string        // "MXN", etc.
	PeriodEndsAt *time.Time     // resulting Gym.SubscriptionEndsAt
	RawPayload   map[string]any // verbatim provider JSON
	OccurredAt   time.Time      // processor-reported timestamp
	RecordedAt   time.Time      // when we persisted the row
}
