// Package repository declares the persistence contracts for the checkins BC.
// Concrete impls live in infraestructure/db/repositories with build tags.
package repository

import (
	"time"

	"github.com/google/uuid"

	checkinDomain "github.com/cuadra/cuadra-core/src/modules/checkins/domain/checkin"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CheckinRepository — UC-029 / UC-030 / UC-032 writes + history reads
// (UC-015 member detail reuses the per-member listing).
type CheckinRepository interface {
	Create(tx sharedDomain.Transaction, c *checkinDomain.Checkin) (*checkinDomain.Checkin, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*checkinDomain.Checkin, error)
	// CountFailedPinAttemptsSince supports DA-32 lockout. Counts checkins
	// whose member is unknown — actually we record only successful PIN
	// matches as checkins; failed attempts live in a kiosko-side in-memory
	// counter (see app/checkin_by_pin.go). Kept here for future "failed
	// attempts" persistence if needed.
	ListByMember(tx sharedDomain.Transaction, memberID uuid.UUID, since time.Time, limit int) ([]*checkinDomain.Checkin, error)
}
