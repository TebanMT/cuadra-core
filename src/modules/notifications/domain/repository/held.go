package repository

import (
	n "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"time"
)

type HeldRepository interface {
	LockHeld(shared.Transaction, uuid.UUID, uuid.UUID) (*n.Notification, error)
	RecoveryRecipient(shared.Transaction, *n.Notification) (string, error)
	OldHeld(shared.Transaction, time.Time, int) ([]*n.Notification, error)
}
