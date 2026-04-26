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
	if _, err := s.enqueueReceipt.Execute(ctx, EnqueueReceiptInput{
		GymID:     evt.GymID,
		PaymentID: evt.PaymentID,
		MemberID:  *evt.MemberID,
		Concept:   evt.Concept,
		Amount:    evt.Amount,
		Folio:     evt.Folio,
	}); err != nil {
		log.Printf("[notifications] enqueue receipt failed: gym=%s payment=%s err=%v",
			evt.GymID, evt.PaymentID, err)
	}
}
