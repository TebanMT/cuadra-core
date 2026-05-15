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
	// CountTodayByGym is the count tile that lives at the top of the
	// reception screen. `today` is interpreted at day granularity in UTC.
	CountTodayByGym(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (int, error)
	// ListRecentByGym returns the latest N checkins for the gym joined with
	// member and operator names so the FE doesn't have to round-trip per
	// row. The active membership's expiry is included for the "vence en X
	// días" badge — same query the kiosk shows after a successful pass.
	ListRecentByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]RecentCheckinRow, error)
	// ListByGymBetween returns checkins in the day-grain window [from,to]
	// (both inclusive). Used by the reports page drill-down ("ver entradas
	// del día X"). Joins member name + expiry the same way ListRecentByGym
	// does so the FE renders one shape.
	ListByGymBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]RecentCheckinRow, error)
	// ListByMemberDetailed returns the latest N checkins de un socio
	// concreto con el operator_name resuelto. Lo consume la pestaña
	// "Asistencia" del detalle de socio para evitar N+1 en el FE.
	ListByMemberDetailed(tx sharedDomain.Transaction, gymID, memberID uuid.UUID, limit int) ([]RecentCheckinRow, error)
}

// RecentCheckinRow is the flat shape consumed by GET /api/v1/checkins
// (UC-032). expiry comes from the member's currently-active membership at
// query time — a recent list is a rolling view, so showing the present
// expiry rather than a snapshot is the more useful behaviour.
type RecentCheckinRow struct {
	ID             uuid.UUID
	MemberID       *uuid.UUID
	MemberName     *string
	Method         string
	Result         string
	ManualOverride bool
	OverrideReason *string
	OperatorName   *string
	CheckinAt      time.Time
	ExpiryDate     *time.Time
}
