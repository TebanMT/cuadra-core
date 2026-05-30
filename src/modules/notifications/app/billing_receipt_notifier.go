package app

import (
	"context"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
)

// BillingReceiptNotifier implementa billingApp.ReceiptNotifier. Envuelve
// EnqueueReceipt para que el BC de billing no importe el paquete notifications.
// Se instancia en main.go y se pasa a billingApp.NewSendReceipt.
type BillingReceiptNotifier struct {
	enqueueReceipt *EnqueueReceipt
}

// NewBillingReceiptNotifier construye el notifier. Se usa en main.go después
// de crear el EnqueueReceipt con su AppBaseURL.
func NewBillingReceiptNotifier(enqueueReceipt *EnqueueReceipt) *BillingReceiptNotifier {
	return &BillingReceiptNotifier{enqueueReceipt: enqueueReceipt}
}

// NotifyReceipt implementa billingApp.ReceiptNotifier. Traduce el input de
// billing en un EnqueueReceiptInput del BC de notificaciones y lo encola.
//
// Para channel="whatsapp_other" se usa el Recipient como número de teléfono
// de destino (PhoneOverride) — útil cuando el operador quiere enviar a un
// número distinto al del socio registrado.
//
// ForceNew=true garantiza que los reenvíos manuales siempre generen una nueva
// fila en notification_queue, independientemente de si ya existía una para el
// mismo pago (idempotencia sólo aplica al flujo automático de evento).
//
// Si no hay MemberID (venta walk-in sin socio), se salta silenciosamente.
func (n *BillingReceiptNotifier) NotifyReceipt(ctx context.Context, in billingApp.ReceiptNotifyInput) (billingApp.ReceiptNotifyOutput, error) {
	if in.MemberID == nil {
		return billingApp.ReceiptNotifyOutput{
			Status: "skipped",
			Note:   "Pago sin socio vinculado; no se puede encolar comprobante WhatsApp.",
		}, nil
	}

	var phoneOverride *string
	if in.Channel == "whatsapp_other" && in.Recipient != "" {
		phoneOverride = &in.Recipient
	}

	result, err := n.enqueueReceipt.Execute(ctx, EnqueueReceiptInput{
		GymID:          in.GymID,
		PaymentID:      in.PaymentID,
		MemberID:       *in.MemberID,
		Concept:        in.Concept,
		Amount:         in.Amount,
		Folio:          in.Folio,
		MembershipType: in.MembershipType,
		PhoneOverride:  phoneOverride,
		ForceNew:       true, // reenvío manual: siempre crear nueva fila
	})
	if err != nil {
		return billingApp.ReceiptNotifyOutput{}, err
	}

	if result.Skipped {
		return billingApp.ReceiptNotifyOutput{
			Status: "skipped",
			Note:   result.SkippedReason,
		}, nil
	}
	return billingApp.ReceiptNotifyOutput{Status: "queued"}, nil
}
