package domain

import (
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"time"
)

const LaunchLimit = 55
const LaunchMonthlyCents int64 = 79900
const LaunchAnnualCents int64 = 720000

// LaunchBenefit is a permanent cohort membership, independent of access expiry.
// Cancelling does not recycle its place or change the billed subscription SKU.
type LaunchBenefit struct {
	GymID       uuid.UUID
	Slot        int
	FirstPaidAt time.Time
}

type LaunchRepository interface {
	// Claim runs in the payment transaction; concurrent callers cannot exceed 55.
	Claim(tx sharedDomain.Transaction, gymID uuid.UUID, paidAt time.Time) (bool, error)
	Remaining(tx sharedDomain.Transaction) (int, error)
}
