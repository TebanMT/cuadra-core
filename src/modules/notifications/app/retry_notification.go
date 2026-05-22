package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RetryNotification re-encola un row failed para que el dispatcher lo
// procese en la siguiente vuelta. Una sola operación owner-only, sin
// reintentos automáticos — el caller hace click cuando regularizó la
// condición que causó el fallo (ej. conectó WhatsApp, corrigió un
// teléfono inválido). Sin esto, los failed quedan acumulándose en la
// cola sin canal claro de recuperación operativa.
type RetryNotification struct {
	Notifications notiRepo.NotificationRepository
	UoW           sharedDomain.UnitOfWork
	Audit         audit.Recorder
	NowFunc       func() time.Time
}

// ErrNotificationNotFailed se devuelve cuando el caller pide retry sobre
// un row que no está en estado failed (typically: pending sigue su
// curso, sent ya se mandó, cancelled fue descartado a propósito). El
// controller lo mapea a 409.
var ErrNotificationNotFailed = errors.New("notification is not in failed state")

func NewRetryNotification(
	repo notiRepo.NotificationRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *RetryNotification {
	return &RetryNotification{
		Notifications: repo,
		UoW:           uow,
		Audit:         recorder,
		NowFunc:       func() time.Time { return time.Now().UTC() },
	}
}

type RetryNotificationInput struct {
	GymID          uuid.UUID
	NotificationID uuid.UUID
	ActorUserID    uuid.UUID
}

func (uc *RetryNotification) Execute(ctx context.Context, in RetryNotificationInput) error {
	if in.GymID == uuid.Nil || in.NotificationID == uuid.Nil {
		return sharedDomain.NewValidationError(errors.New("gym_id and notification_id are required"))
	}
	now := uc.NowFunc()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		n, err := uc.Notifications.GetByID(tx, in.NotificationID)
		if err != nil {
			return err
		}
		if n.GymID != in.GymID {
			return sharedDomain.NewBusinessError(errors.New("cross-gym"), "")
		}
		if n.Status != notification.StatusFailed {
			return sharedDomain.NewBusinessError(ErrNotificationNotFailed, "Solo puedes reintentar mensajes que fallaron.")
		}

		// Re-arm: status → pending, ScheduledFor → now para que la
		// próxima vuelta del dispatcher lo procese, RetryCount conservado
		// (señal de cuántos intentos automáticos llevó antes), ErrorMessage
		// limpiado para que el FE no muestre "último error" desactualizado.
		n.Status = notification.StatusPending
		n.ScheduledFor = now
		n.FailedAt = nil
		n.ErrorMessage = nil
		n.Version++
		n.UpdatedAt = now
		if _, err := uc.Notifications.Update(tx, n); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		actor := in.ActorUserID
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "notification_queue",
			EntityID:    n.ID,
			Action:      "notification_retry",
			ActorUserID: &actor,
			Changes: map[string]any{
				"template_key": n.TemplateKey,
				"channel":      n.Channel,
				"retry_count":  n.RetryCount,
			},
			At: now,
		})
		return nil
	})
}
