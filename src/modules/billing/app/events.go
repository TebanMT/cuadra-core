package app

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PaymentCompletedEvent is emitted by UC-018 / UC-025 inside the same UoW
// transaction that committed the Payment. The notifications BC (Sesión 7)
// will subscribe and decide whether to send a WhatsApp receipt.
//
// In Sesión 3 there is no event bus yet; the use cases call publisher.Publish
// with a no-op default (NoopPublisher) and the event is logged for manual
// inspection. Wiring a real bus is a future TODO.
type PaymentCompletedEvent struct {
	GymID          uuid.UUID
	PaymentID      uuid.UUID
	MemberID       *uuid.UUID
	Concept        string
	Amount         float64
	Folio          string
	OperatorID     uuid.UUID
	BalancePending float64
	// MembershipTypeName y NewExpiry alimentan el recibo de WhatsApp con el
	// plan y la vigencia REALES (antes el recibo hardcodeaba "Mensual" y
	// aproximaba hoy+30d). Sólo se llenan para pagos de membresía; en ventas
	// de producto quedan en su zero-value (string vacío / nil).
	MembershipTypeName string
	NewExpiry          *time.Time
}

// EventPublisher abstracts the future notifications bus. NoopPublisher is the
// MVP default — it discards events. Tests may inject a fake to assert.
type EventPublisher interface {
	PublishPaymentCompleted(ctx context.Context, evt PaymentCompletedEvent)
}

type NoopPublisher struct{}

func (NoopPublisher) PublishPaymentCompleted(context.Context, PaymentCompletedEvent) {}
