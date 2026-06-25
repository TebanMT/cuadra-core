// Package repository declares the persistence contracts for the members BC.
// Concrete impls live in infraestructure/db/repositories with build tags.
package repository

import (
	"time"

	"github.com/google/uuid"

	caDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/contact_attempt"
	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
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
	// CountByGym returns the number of (non-deleted) membership types for a gym.
	// Used by GET /gyms/me/setup-status to decide the next setup step.
	CountByGym(tx sharedDomain.Transaction, gymID uuid.UUID) (int, error)
}

// MemberRepository — UC-012..UC-016, UC-032, UC-035.
type MemberRepository interface {
	Create(tx sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error)
	Update(tx sharedDomain.Transaction, m *memberDomain.Member) (*memberDomain.Member, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*memberDomain.Member, error)
	ExistsByGymAndPhone(tx sharedDomain.Transaction, gymID uuid.UUID, phone string) (bool, error)
	// MemberNumberExistsInGym reports whether ANOTHER non-deleted member in
	// `gymID` already holds `number` (ADR-010). O(1) — the unique index
	// `uq_members_gym_number` backs it. Used by AssignMemberNumber to honor
	// an operator-supplied number and as a defense check before persisting a
	// generated one. excludeMemberID lets a member re-claim its own number.
	MemberNumberExistsInGym(tx sharedDomain.Transaction, gymID uuid.UUID, number int, excludeMemberID *uuid.UUID) (bool, error)
	// ListUsedMemberNumbers returns every member_number currently in use in
	// `gymID` (non-deleted, non-null). The asignador uses it for the bump
	// decision (count) and gap-fill (lowest free slot). "Usados" = no
	// borrados; reuse de inactivos >1 año queda diferido (ADR-010 fase 7).
	ListUsedMemberNumbers(tx sharedDomain.Transaction, gymID uuid.UUID) ([]int, error)
	// NextFolio mints the next folio (gym_001, gym_002, ...). Implementations
	// SHOULD use SELECT ... FOR UPDATE (Postgres) or SQLite's serialised tx
	// guarantees so concurrent creates don't collide.
	NextFolio(tx sharedDomain.Transaction, gymID uuid.UUID) (string, error)
	// List supports UC-014. Returns rows + total. statusFilter is one of
	// "" / "active" / "expiring_soon" / "expired" / "inactive".
	List(tx sharedDomain.Transaction, q ListQuery) ([]*MemberWithMembership, int, error)
	GetWithCurrentMembership(tx sharedDomain.Transaction, gymID, memberID uuid.UUID) (*MemberWithMembership, error)
	// GetNamesByIDs returns id→full_name for the given members. Missing IDs
	// (deleted, or other gym) are simply omitted from the result map. Used by
	// cross-context read paths (e.g. billing's gym-wide cobros list) to avoid
	// the N+1 of GetByID per row.
	GetNamesByIDs(tx sharedDomain.Transaction, ids []uuid.UUID) (map[uuid.UUID]string, error)
	// GetContactsByIDs returns id/full_name/phone for the given members WITHIN
	// the gym. Missing, soft-deleted, or cross-gym IDs are omitted silently —
	// el filtro por gymID es el cerrojo anti cross-tenant. Lo usa la selección
	// manual de socios del broadcast (UC-041). Phone puede venir vacío (el
	// caller lo descarta, igual que en List).
	GetContactsByIDs(tx sharedDomain.Transaction, gymID uuid.UUID, ids []uuid.UUID) ([]MemberContact, error)
}

// MemberContact es una proyección liviana (id + nombre + teléfono) para
// lecturas en bloque, p.ej. resolver destinatarios de un broadcast por IDs
// seleccionados sin el N+1 de GetByID.
type MemberContact struct {
	ID       uuid.UUID
	FullName string
	Phone    string
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

// MemberNumberFinder is the optional capability the checkins BC asks from
// MemberRepository for check-in-by-number (ADR-010). Both Postgres and
// SQLite member repos implement it. FindByMemberNumber does an O(1) lookup
// on `uq_members_gym_number` and returns (nil, nil) when no non-deleted
// member in the gym holds that number — letting checkins distinguish "no
// such number" from an infra error without importing the members errors.
//
// It deliberately does NOT filter by status: an inactive socio entering a
// valid number must resolve to denied_inactive (with their name) rather
// than "número incorrecto" — same rationale as the old ListPinCandidates.
type MemberNumberFinder interface {
	FindByMemberNumber(tx sharedDomain.Transaction, gymID uuid.UUID, number int) (*memberDomain.Member, error)
}

// ContactAttemptRepository — UC-035 append-only history of "le hablé".
type ContactAttemptRepository interface {
	Create(tx sharedDomain.Transaction, a *caDomain.ContactAttempt) (*caDomain.ContactAttempt, error)
	// LastByMember returns the most recent attempt for `memberID`, or
	// (nil, nil) when none. Reports use this to filter "vencidos sin
	// contactar en últimos 7 días".
	LastByMember(tx sharedDomain.Transaction, memberID uuid.UUID) (*caDomain.ContactAttempt, error)
	ListByMember(tx sharedDomain.Transaction, memberID uuid.UUID, limit int) ([]*caDomain.ContactAttempt, error)
}

// FingerprintRepository — UC-028 + the read paths checkins/Sesión 5 needs.
//
// Templates are stored *encrypted* (ADR-006 §2). The repo doesn't know the
// GMK; it just shovels bytes. Decryption happens in the Reader implementation
// inside the kiosko process.
type FingerprintRepository interface {
	Create(tx sharedDomain.Transaction, fp *fpDomain.MemberFingerprint) (*fpDomain.MemberFingerprint, error)
	// Update bumps version + persists soft-delete or re-enrollment.
	Update(tx sharedDomain.Transaction, fp *fpDomain.MemberFingerprint) (*fpDomain.MemberFingerprint, error)
	// ListByMember returns every active (deleted_at IS NULL) fingerprint
	// enrolled for a member — up to MaxFingerprintsPerMember templates of the
	// same finger (UC-028). Empty slice when none are enrolled. UC-028 uses it
	// to reject duplicate registrations.
	ListByMember(tx sharedDomain.Transaction, memberID uuid.UUID) ([]*fpDomain.MemberFingerprint, error)
	// ListByGym returns every active (deleted_at IS NULL) fingerprint in the
	// gym. The kiosko loads this once at boot and again whenever a new
	// enrollment fires (UC-029 step 4 needs the full candidate set in memory).
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*fpDomain.MemberFingerprint, error)
}
