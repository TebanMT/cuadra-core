package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// OperatorWelcomePINDispatch espeja a WelcomePinDispatchResult del lado de
// members. Lo replicamos en users para no acoplar BCs vía importes
// cruzados — el notifier real (notifications/app/EnqueueOperatorWelcomePIN)
// implementa la interface OperatorWelcomePINNotifier definida abajo.
//
//   - Dispatched: true si una fila en notification_queue quedó persistida
//     (o ya existía por la clave de idempotencia). El delivery final lo
//     reintenta el dispatcher de WhatsApp.
//   - SkippedReason: poblado SOLO cuando Dispatched es false. Strings
//     estables: "whatsapp_not_connected", "no_user_phone", "cross_gym",
//     "notifier_not_wired".
//   - RecipientPhone: el número al que se envió (eco para el FE).
type OperatorWelcomePINDispatch struct {
	Dispatched     bool
	SkippedReason  string
	RecipientPhone string
}

// OperatorWelcomePINNotifier es el seam que CreateOperator y
// RotateOperatorPIN llaman después de asignar/rotar el PIN. El impl
// real (notifications/app) DEBE usar la transacción provista para que
// el enqueue sea atómico con el cambio de pin_hash — un fallo parcial
// no debe dejar el PIN guardado sin notification encolada.
type OperatorWelcomePINNotifier interface {
	Notify(ctx context.Context, tx sharedDomain.Transaction, in OperatorWelcomePINNotifyInput, now time.Time) (OperatorWelcomePINDispatch, error)
}

// OperatorWelcomePINNotifyInput es el payload que users le pasa al
// notifier. PIN es el plaintext de 4 dígitos (el mismo que se devuelve
// al FE en la respuesta del 201 / rotate).
// WelcomeImageURL es la URL pública del banner con el PIN embebido,
// generado y subido a R2 antes de llamar Notify. Si está vacío, el
// dispatcher omite el header de imagen (degraded: solo texto).
type OperatorWelcomePINNotifyInput struct {
	GymID           uuid.UUID
	UserID          uuid.UUID
	PIN             string
	WelcomeImageURL string
}

// noopOperatorWelcomePINNotifier es el default que usan los constructores
// cuando el caller no inyectó un notifier real (típico en unit tests que
// no se preocupan por WhatsApp). Devuelve "skipped" para que los consumers
// puedan ramificar sin nil checks en cada lugar.
type noopOperatorWelcomePINNotifier struct{}

func (noopOperatorWelcomePINNotifier) Notify(context.Context, sharedDomain.Transaction, OperatorWelcomePINNotifyInput, time.Time) (OperatorWelcomePINDispatch, error) {
	return OperatorWelcomePINDispatch{Dispatched: false, SkippedReason: "notifier_not_wired"}, nil
}
