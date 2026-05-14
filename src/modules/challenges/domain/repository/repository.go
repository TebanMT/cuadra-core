// Package repository declares the persistence contract for the challenges
// (retos) bounded context. Concrete impls live in infraestructure/db/
// repositories (server build tag = GORM/Postgres; sidecar build tag =
// sqlx/SQLite + sync queue enqueue).
package repository

import (
	"time"

	"github.com/google/uuid"

	categoryDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/category"
	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ChallengeRepository is the aggregate root's persistence boundary.
type ChallengeRepository interface {
	Create(tx sharedDomain.Transaction, c *challengeDomain.Challenge) (*challengeDomain.Challenge, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*challengeDomain.Challenge, error)
	Update(tx sharedDomain.Transaction, c *challengeDomain.Challenge) (*challengeDomain.Challenge, error)
	// ListByGym returns the gym's challenges newest-first, with optional
	// status filter. Pass empty string for "all statuses". Skips
	// soft-deleted rows.
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, statusFilter string) ([]*challengeDomain.Challenge, error)
}

// CategoryRepository handles challenge_categories.
type CategoryRepository interface {
	Create(tx sharedDomain.Transaction, c *categoryDomain.Category) (*categoryDomain.Category, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*categoryDomain.Category, error)
	Update(tx sharedDomain.Transaction, c *categoryDomain.Category) (*categoryDomain.Category, error)
	SoftDelete(tx sharedDomain.Transaction, id uuid.UUID) error
	ListByChallenge(tx sharedDomain.Transaction, challengeID uuid.UUID) ([]*categoryDomain.Category, error)
	// CountParticipants is the precondition for SoftDelete — callers must
	// reject the deletion when this is > 0 (ErrCategoryHasParticipants).
	CountParticipants(tx sharedDomain.Transaction, categoryID uuid.UUID) (int, error)
}

// ParticipantRepository handles challenge_participants.
type ParticipantRepository interface {
	Create(tx sharedDomain.Transaction, p *participantDomain.Participant) (*participantDomain.Participant, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*participantDomain.Participant, error)
	Update(tx sharedDomain.Transaction, p *participantDomain.Participant) (*participantDomain.Participant, error)
	SoftDelete(tx sharedDomain.Transaction, id uuid.UUID) error
	// ListByChallenge returns all non-deleted participants for a challenge,
	// optionally filtered by status and/or category.
	ListByChallenge(tx sharedDomain.Transaction, challengeID uuid.UUID, statusFilter string, categoryFilter *uuid.UUID) ([]*participantDomain.Participant, error)
	// ExistsByMember answers the "already participating?" guard at registration.
	ExistsByMember(tx sharedDomain.Transaction, challengeID, memberID uuid.UUID) (bool, error)
	// HasAnyMeasurement is the precondition for SoftDelete (recommend
	// Withdraw instead) and for editing the participant's exercise list
	// after T₀ has been captured.
	HasAnyMeasurement(tx sharedDomain.Transaction, participantID uuid.UUID) (bool, error)
}

// MeasurementRepository handles challenge_measurements. Writes are
// append-only in spirit: corrections insert a new row + mark the prior
// as superseded in the same transaction.
type MeasurementRepository interface {
	Create(tx sharedDomain.Transaction, m *measurementDomain.Measurement) (*measurementDomain.Measurement, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*measurementDomain.Measurement, error)
	// Supersede atomically points priorID at replacementID and sets the
	// superseded_at timestamp. Repo MUST run this in the same transaction
	// as the Create of the replacement, hence the tx parameter (not a
	// separate UoW call).
	Supersede(tx sharedDomain.Transaction, priorID, replacementID uuid.UUID, at time.Time) error
	// ListByParticipant returns all rows (active + superseded) ordered by
	// created_at DESC. The capture flow uses this for the "ya existe una
	// medición" warning + audit display.
	ListByParticipant(tx sharedDomain.Transaction, participantID uuid.UUID) ([]*measurementDomain.Measurement, error)
	// GetActiveByMoment returns the single active measurement for
	// (participant, moment), or nil + ok=false if none exists. Used by
	// the ranking calculation (needs the active T₀ + active T₁ per
	// participant) and by the supersession path (find the existing row
	// to replace).
	GetActiveByMoment(tx sharedDomain.Transaction, participantID uuid.UUID, moment string) (*measurementDomain.Measurement, bool, error)
	// CountByChallenge returns the number of active measurements across
	// the challenge — used as the "any measurement captured?" check that
	// locks config edits (ErrConfigLocked).
	CountByChallenge(tx sharedDomain.Transaction, challengeID uuid.UUID) (int, error)
}

// AttendanceCounter reports how many check-ins a member logged inside a
// challenge's running window. Implemented by the checkins module's repo
// (we don't import that repo directly — DDD keeps modules independent;
// the wiring layer in main.go adapts checkins.Repository to this
// interface).
type AttendanceCounter interface {
	CountInRange(tx sharedDomain.Transaction, gymID, memberID uuid.UUID, fromMs, toMs int64) (int, error)
}
