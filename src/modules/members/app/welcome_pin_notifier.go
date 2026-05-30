package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// WelcomePinDispatchResult describes whether the welcome-PIN notification
// could be enqueued for outbound delivery. The notifications BC is the
// canonical decider; this struct is the boundary contract the members BC
// reads back from the notifier seam without importing the notifications
// package.
//
//   - Dispatched: true when a notification_queue row was inserted (or
//     already existed thanks to the idempotency key). The actual delivery
//     is best-effort — the WhatsApp dispatcher retries on transient
//     failure.
//   - SkippedReason: only populated when Dispatched is false. Stable
//     strings the FE / logs can branch on:
//     "whatsapp_not_connected", "no_member_phone", "disabled_by_gym",
//     "cross_gym".
//   - RecipientPhone: the E.164-ish phone the message went out to (echoed
//     back so the FE can render "enviado a +52…").
type WelcomePinDispatchResult struct {
	Dispatched     bool
	SkippedReason  string
	RecipientPhone string
}

// WelcomePinNotifier is the cross-BC seam the members use cases call when
// auto-assigning or regenerating a socio's PIN. The notifications BC
// implements it (see notifications/app/enqueue_welcome_pin.go); main.go
// wires the concrete impl at boot.
//
// The implementation MUST use the passed transaction so the enqueue is
// atomic with the PIN assignment — otherwise a partial failure could
// commit the PIN to members.* but lose the notification row. CreateMember
// runs this inside its Command tx.
//
// Returning nil error + Dispatched=false is the explicit "skipped"
// signal. Errors are reserved for unexpected infra failures.
type WelcomePinNotifier interface {
	Notify(ctx context.Context, tx sharedDomain.Transaction, in WelcomePinNotifyInput, now time.Time) (WelcomePinDispatchResult, error)
}

// WelcomePinNotifyInput is the payload members hands to the notifier.
// Pin is the plaintext 4-digit code (the same one returned to the FE).
// WelcomeImageURL es la URL pública del banner con el PIN embebido,
// generado y subido a R2 antes de llamar Notify. Si está vacío, el
// dispatcher omite el header de imagen (degraded: solo texto).
type WelcomePinNotifyInput struct {
	GymID           uuid.UUID
	MemberID        uuid.UUID
	Pin             string
	WelcomeImageURL string
}

// noopWelcomePinNotifier is the default the constructors use when the
// caller didn't wire a real notifier (typical in unit tests that don't
// care about WhatsApp). It returns "disabled" so consumers can branch
// without nil checks at every call site.
type noopWelcomePinNotifier struct{}

func (noopWelcomePinNotifier) Notify(context.Context, sharedDomain.Transaction, WelcomePinNotifyInput, time.Time) (WelcomePinDispatchResult, error) {
	return WelcomePinDispatchResult{Dispatched: false, SkippedReason: "notifier_not_wired"}, nil
}
