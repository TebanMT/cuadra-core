package app

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PaymentReceiptNotifier es el seam cross-BC para encolar el RECIBO del primer
// pago del alta. La BC de notifications lo implementa (envuelve EnqueueReceipt);
// main.go cablea el impl concreto al boot.
//
// CreateMember cobra el primer pago DIRECTO (no vía RegisterMembershipPayment,
// que renovaría duplicando la membresía recién creada), así que no pasa por el
// EventPublisher de billing. Este seam cierra ese hueco: el recibo del primer
// pago se encola con los mismos datos que el de renovación.
//
// Se invoca DESPUÉS de que la tx del alta commitea — best-effort, igual que el
// BillingEventSubscriber: si falla, el pago ya quedó guardado y el dispatcher
// reintenta la entrega. Por eso NO recibe la tx ni devuelve error (sólo loguea
// adentro). Nil = no se envía (tests / builds sin la BC notifications).
type PaymentReceiptNotifier interface {
	NotifyPaymentReceipt(ctx context.Context, in PaymentReceiptInput)
}

// PaymentReceiptInput espeja los campos que EnqueueReceipt (UC-039) necesita
// para renderizar el recibo: el plan real + la vigencia nueva, no aproximados.
type PaymentReceiptInput struct {
	GymID              uuid.UUID
	PaymentID          uuid.UUID
	MemberID           uuid.UUID
	Concept            string
	Amount             float64
	Folio              string
	MembershipTypeName string
	NewExpiry          *time.Time
}

// noopPaymentReceiptNotifier es el default cuando el caller no cableó un
// notifier real — no hace nada (mismo patrón que noopWelcomeNotifier).
type noopPaymentReceiptNotifier struct{}

func (noopPaymentReceiptNotifier) NotifyPaymentReceipt(context.Context, PaymentReceiptInput) {}
