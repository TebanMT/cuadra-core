package app

import (
	"context"
	"log"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
)

// FirstPaymentReceiptNotifier implementa members/app.PaymentReceiptNotifier:
// encola el recibo del PRIMER pago del alta (CreateMember), que cobra directo
// sin pasar por el EventPublisher de billing. Es el gemelo del
// BillingEventSubscriber (que cubre el recibo de renovación) — mismo mapeo a
// EnqueueReceiptInput, misma política best-effort (errores logueados, no
// propagados: el pago ya commiteó y el dispatcher reintenta la entrega).
type FirstPaymentReceiptNotifier struct {
	enqueueReceipt *EnqueueReceipt
}

func NewFirstPaymentReceiptNotifier(enqueueReceipt *EnqueueReceipt) *FirstPaymentReceiptNotifier {
	return &FirstPaymentReceiptNotifier{enqueueReceipt: enqueueReceipt}
}

func (n *FirstPaymentReceiptNotifier) NotifyPaymentReceipt(ctx context.Context, in memApp.PaymentReceiptInput) {
	if n == nil || n.enqueueReceipt == nil {
		return
	}
	if _, err := n.enqueueReceipt.Execute(ctx, EnqueueReceiptInput{
		GymID:              in.GymID,
		PaymentID:          in.PaymentID,
		MemberID:           in.MemberID,
		Concept:            in.Concept,
		Amount:             in.Amount,
		Folio:              in.Folio,
		MembershipTypeName: in.MembershipTypeName,
		NewExpiry:          in.NewExpiry,
	}); err != nil {
		log.Printf("[notifications] enqueue first-payment receipt failed: gym=%s payment=%s err=%v",
			in.GymID, in.PaymentID, err)
	}
}
