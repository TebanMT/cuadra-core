// Package repository declares the persistence contracts for the members BC.
// Concrete impls live in infraestructure/db/repositories with build tags.
package repository

import (
	"time"

	"github.com/google/uuid"

	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// MembershipTypeRepository — UC-011.
type MembershipTypeRepository interface {
	Create(tx sharedDomain.Transaction, mt *mtDomain.MembershipType) (*mtDomain.MembershipType, error)
	Update(tx sharedDomain.Transaction, mt *mtDomain.MembershipType) (*mtDomain.MembershipType, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*mtDomain.MembershipType, error)
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, includeInactive bool) ([]*mtDomain.MembershipType, error)
	ExistsByGymAndName(tx sharedDomain.Transaction, gymID uuid.UUID, name string) (bool, error)
	// CountActiveMembershipsByType returns how many memberships (status=active)
	// reference this type. Used to decide whether deactivation is the only option.
	CountActiveMembershipsByType(tx sharedDomain.Transaction, typeID uuid.UUID) (int, error)
}

// MemberRepository — UC-012..UC-016, UC-032, UC-035.
type MemberRepository interface {
	Create(tx sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error)
	Update(tx sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*memberDomain.Member, error)
	ExistsByGymAndPhone(tx sharedDomain.Transaction, gymID uuid.UUID, phone string) (bool, error)
	// ExistsPinHashInGym checks whether any other member in `gymID` already has
	// the given bcrypt PIN hash stored. NOTE: bcrypt salts make exact-match
	// detection impossible — this method MUST iterate hashes in the gym and
	// run bcrypt.Verify against the plain pin. The caller passes plainPin.
	PinHashCollidesInGym(tx sharedDomain.Transaction, gymID uuid.UUID, plainPin string, excludeMemberID *uuid.UUID) (bool, error)
	// NextFolio mints the next folio (gym_001, gym_002, ...). Implementations
	// SHOULD use SELECT ... FOR UPDATE (Postgres) or SQLite's serialised tx
	// guarantees so concurrent creates don't collide.
	NextFolio(tx sharedDomain.Transaction, gymID uuid.UUID) (string, error)
	// List supports UC-014. Returns rows + total. statusFilter is one of
	// "" / "active" / "expiring_soon" / "expired" / "inactive".
	List(tx sharedDomain.Transaction, q ListQuery) ([]*MemberWithMembership, int, error)
	GetWithCurrentMembership(tx sharedDomain.Transaction, gymID, memberID uuid.UUID) (*MemberWithMembership, error)
}

// ListQuery is the input for UC-014.
type ListQuery struct {
	GymID         uuid.UUID
	Search        string // matches LOWER(full_name) prefix OR phone suffix
	StatusFilter  string // see MemberRepository.List
	PlanID        *uuid.UUID
	Sort          string // "name" | "expiry" | "created_at" | "last_checkin"
	SortAscending bool
	Page          int
	PageSize      int
	Today         time.Time
}

// MemberWithMembership is the read model for UC-014 / UC-015 — the Member
// joined with its current Membership (if any).
type MemberWithMembership struct {
	Member            *memberDomain.Member
	CurrentMembership *membershipDomain.Membership
}

// MembershipRepository — backs `Membership` aggregate writes.
type MembershipRepository interface {
	Create(tx sharedDomain.Transaction, m *membershipDomain.Membership) (*membershipDomain.Membership, error)
	Update(tx sharedDomain.Transaction, m *membershipDomain.Membership) (*membershipDomain.Membership, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*membershipDomain.Membership, error)
	GetCurrentByMember(tx sharedDomain.Transaction, memberID uuid.UUID) (*membershipDomain.Membership, error)
	// GetReplacedBy returns the Membership row whose ReplacedBy points at the
	// given id (i.e. the predecessor of the current row). Returns (nil, nil)
	// when there is no predecessor — first-ever membership for the member.
	// Used by UC-022 to restore the prior membership on refund.
	GetReplacedBy(tx sharedDomain.Transaction, replacementID uuid.UUID) (*membershipDomain.Membership, error)
}

// MembershipAdjustmentRepository — append-only history (UC-017).
type MembershipAdjustmentRepository interface {
	Create(tx sharedDomain.Transaction, a *membershipDomain.MembershipAdjustment) (*membershipDomain.MembershipAdjustment, error)
	ListByMembership(tx sharedDomain.Transaction, membershipID uuid.UUID) ([]*membershipDomain.MembershipAdjustment, error)
}
