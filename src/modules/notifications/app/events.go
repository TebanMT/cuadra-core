// Package app holds the notifications BC use cases. Naming mirrors the rest
// of the codebase: one file per use case, struct + Constructor + Execute.
//
// Events are wired here too — `BillingEventSubscriber` plugs into the
// existing billing.EventPublisher interface and translates PaymentCompleted
// into UC-039 EnqueueReceipt invocations. main.go injects this subscriber
// into billing's RegisterMembershipPayment / RegisterSale.
package app

import (
	"context"
	"log"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
)

// BillingEventSubscriber implements billing.EventPublisher. It sits in the
// notifications BC because the notifications BC owns "what to do when
// billing emits an event". Billing only knows about the publisher seam.
type BillingEventSubscriber struct {
	enqueueReceipt *EnqueueReceipt
}

// NewBillingEventSubscriber wires the subscriber. In tests pass a fake
// EnqueueReceipt; in main.go pass the real one.
func NewBillingEventSubscriber(enqueueReceipt *EnqueueReceipt) *BillingEventSubscriber {
	return &BillingEventSubscriber{enqueueReceipt: enqueueReceipt}
}

// PublishPaymentCompleted is the billing.EventPublisher implementation.
// Errors here are swallowed (logged) — a failed receipt enqueue must not
// roll back the payment that already committed. The dispatcher's retry
// logic handles delivery; this just inserts the queue row.
func (s *BillingEventSubscriber) PublishPaymentCompleted(ctx context.Context, evt billingApp.PaymentCompletedEvent) {
	if s == nil || s.enqueueReceipt == nil || evt.MemberID == nil {
		return
	}
	if _, err := s.enqueueReceipt.Execute(ctx, toEnqueueReceiptInput(evt, false)); err != nil {
		log.Printf("[notifications] enqueue receipt failed: gym=%s payment=%s err=%v",
			evt.GymID, evt.PaymentID, err)
	}
}

// ResendReceipt implementa billing.ReceiptResender (UC-020, reenvío
// manual). Mismo mapeo de evento que el publish del cobro, pero en modo
// resend y CON veredicto: el operador que aprieta "Enviar por WhatsApp"
// merece saber si se encoló, si ya iba en camino, o si falta el teléfono —
// el fire-and-forget de antes convertía todo eso en un "éxito" ciego.
func (s *BillingEventSubscriber) ResendReceipt(ctx context.Context, evt billingApp.PaymentCompletedEvent) (billingApp.ReceiptResendOutcome, error) {
	if s == nil || s.enqueueReceipt == nil {
		return billingApp.ReceiptResendOutcome{Status: "queued"}, nil
	}
	if evt.MemberID == nil {
		return billingApp.ReceiptResendOutcome{Status: "skipped", Reason: "no_member"}, nil
	}
	out, err := s.enqueueReceipt.Execute(ctx, toEnqueueReceiptInput(evt, true))
	if err != nil {
		return billingApp.ReceiptResendOutcome{}, err
	}
	switch {
	case out.Skipped:
		return billingApp.ReceiptResendOutcome{Status: "skipped", Reason: out.SkippedReason}, nil
	case out.AlreadyPending:
		return billingApp.ReceiptResendOutcome{Status: "already_pending"}, nil
	default:
		return billingApp.ReceiptResendOutcome{Status: "queued"}, nil
	}
}

func toEnqueueReceiptInput(evt billingApp.PaymentCompletedEvent, resend bool) EnqueueReceiptInput {
	return EnqueueReceiptInput{
		GymID:              evt.GymID,
		PaymentID:          evt.PaymentID,
		MemberID:           *evt.MemberID,
		Concept:            evt.Concept,
		Amount:             evt.Amount,
		Folio:              evt.Folio,
		MembershipTypeName: evt.MembershipTypeName,
		NewExpiry:          evt.NewExpiry,
		Resend:             resend,
	}
}
