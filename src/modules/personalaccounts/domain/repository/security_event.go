package repository

import (
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"time"
)

// SecurityEvent intentionally has no arbitrary payload: OTP, phone and tokens
// cannot be included in its minimal authentication audit record.
type SecurityEvent struct {
	ID            uuid.UUID
	AccountID     uuid.UUID
	ActorDeviceID uuid.UUID
	OperationID   uuid.UUID
	Type          string
	OccurredAt    time.Time
}
type SecurityEventRepository interface {
	Append(shared.Transaction, SecurityEvent) error
}
