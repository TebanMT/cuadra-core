package app

import (
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ExpiryCandidate is a row consumed by EnqueueExpiryReminder. The reader
// pulls this projection straight from the join (members + memberships +
// gyms) so the use case stays decoupled from the storage shape.
type ExpiryCandidate struct {
	GymID          uuid.UUID
	GymName        string
	MemberID       uuid.UUID
	MemberFullName string
	MemberPhone    string
	MembershipType string
	ExpiryDate     time.Time
}

// ExpiryReader is the cross-context query surface used by UC-038. It lives
// in the notifications BC because nothing else needs it; the implementation
// (queries_postgres.go) joins members + memberships + gyms.
type ExpiryReader interface {
	// FindExpiringOn returns active members whose CURRENT membership has
	// expiry_date == `targetDate` (UTC date-only). The use case calls it
	// three times per tick: targetDate = today-3d, today, today+5d (post-
	// expiry chase, but only members who haven't renewed since).
	FindExpiringOn(tx sharedDomain.Transaction, targetDate time.Time) ([]ExpiryCandidate, error)
}
