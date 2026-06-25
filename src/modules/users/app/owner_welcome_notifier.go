package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// OwnerWelcomeDispatch espeja el resultado del encolado del banner "tu sistema
// ya está vivo" del dueño. El notifier real (notifications/app/EnqueueOwnerWelcome)
// implementa OwnerWelcomeNotifier; se replica acá para no acoplar BCs.
//
//   - Dispatched: true si quedó una fila en notification_queue (o ya existía
//     por la clave de idempotencia).
//   - SkippedReason: poblado sólo cuando Dispatched=false. Strings estables:
//     "no_user_phone", "cross_gym", "notifier_not_wired".
//   - RecipientPhone: el número al que se envió (eco).
type OwnerWelcomeDispatch struct {
	Dispatched     bool
	SkippedReason  string
	RecipientPhone string
}

// OwnerWelcomeNotifier es el seam que RedeemInstallerBootstrap llama en el
// primer link del primer dispositivo. El impl real DEBE usar la transacción
// provista para que el enqueue sea atómico con la regeneración del código de
// acceso del dueño + el flag de fire-once del gym.
type OwnerWelcomeNotifier interface {
	Notify(ctx context.Context, tx sharedDomain.Transaction, in OwnerWelcomeNotifyInput, now time.Time) (OwnerWelcomeDispatch, error)
}

// OwnerWelcomeNotifyInput — PIN es el código de acceso plaintext recién
// regenerado (el mismo que se hornea en el banner del dueño).
type OwnerWelcomeNotifyInput struct {
	GymID  uuid.UUID
	UserID uuid.UUID
	PIN    string
}

// noopOwnerWelcomeNotifier es el default cuando no se inyectó un notifier real
// (tests / builds sin notificaciones).
type noopOwnerWelcomeNotifier struct{}

func (noopOwnerWelcomeNotifier) Notify(context.Context, sharedDomain.Transaction, OwnerWelcomeNotifyInput, time.Time) (OwnerWelcomeDispatch, error) {
	return OwnerWelcomeDispatch{Dispatched: false, SkippedReason: "notifier_not_wired"}, nil
}
