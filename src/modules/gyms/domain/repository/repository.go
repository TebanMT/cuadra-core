// Package repository declares the persistence contract for the gyms BC.
// Concrete implementations live in infraestructure/db/repositories under
// build tags `server` (GORM/Postgres) or `sidecar` (sqlx/SQLite).
package repository

import (
	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type GymRepository interface {
	Create(tx sharedDomain.Transaction, g *gymDomain.Gym) (*gymDomain.Gym, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*gymDomain.Gym, error)
	Update(tx sharedDomain.Transaction, g *gymDomain.Gym) (*gymDomain.Gym, error)
	HasMembershipType(tx sharedDomain.Transaction, gymID uuid.UUID) (bool, error)
}

type OwnershipTransferRepository interface {
	RecordTransfer(tx sharedDomain.Transaction, gymID, fromUserID, toUserID uuid.UUID) (uuid.UUID, error)
}

type OwnershipOTPRepository interface {
	Save(tx sharedDomain.Transaction, otp OTPRecord) error
	Consume(tx sharedDomain.Transaction, gymID, fromUserID uuid.UUID, plainCode string) (*OTPRecord, error)
}

// OTPRecord is the in-memory shape of a one-shot transfer code (UC-010). The
// repo hashes the plain code before storage; the consume path re-hashes the
// candidate and compares.
type OTPRecord struct {
	ID         uuid.UUID
	GymID      uuid.UUID
	FromUserID uuid.UUID
	ToUserID   uuid.UUID
	PlainCode  string // only set when issuing
	CodeHash   []byte
	ExpiresAt  int64 // unix seconds
}
