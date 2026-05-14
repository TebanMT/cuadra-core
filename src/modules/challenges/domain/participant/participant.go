// Package participant — a Member's enrolment in a Challenge under one of
// the challenge's Categories. Owns fee / status / exercise selections.
package participant

import (
	"strings"
	"time"

	"github.com/google/uuid"

	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
)

// Status values match the postgres CHECK constraint exactly.
const (
	StatusRegistered   = "registered"
	StatusActive       = "active"
	StatusDisqualified = "disqualified"
	StatusCompleted    = "completed"
	StatusWithdrew     = "withdrew"
)

// Exercise pattern constants. Free-form on the wire (the DB column is
// TEXT) but we centralise the canonical labels here so the FE picker
// and the BE validations agree without a separate ENUM migration.
const (
	ExerciseLegsSquat          = "sentadilla"
	ExerciseLegsLegPress       = "prensa"
	ExerciseLegsRomanianDL     = "peso_muerto_rumano"
	ExercisePushBenchPress     = "press_banca"
	ExercisePushChestPressMach = "press_pecho_maquina"
	ExercisePushOverheadPress  = "press_militar"
	ExercisePullDeadlift       = "peso_muerto"
	ExercisePullLatPulldown    = "jalon_polea"
	ExercisePullRow            = "remo"
)

type Participant struct {
	ID                    uuid.UUID
	GymID                 uuid.UUID
	ChallengeID           uuid.UUID
	MemberID              uuid.UUID
	CategoryID            uuid.UUID
	Version               int
	ExerciseLegs          string
	ExercisePush          string
	ExercisePull          string
	InscriptionFeePaid    bool
	InscriptionPaidAt     *time.Time
	InscriptionRefundedAt *time.Time
	Status                string
	DisqualificationReason string
	DisqualifiedAt        *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
	DeletedAt             *time.Time
}

// NewParticipant constructs a registered participant. Exercise choices
// are optional at registration time (defaults documented in the FE
// AgregarParticipanteModal) — the use case will fill them with defaults
// when blank, so this entity stays permissive.
func NewParticipant(id, gymID, challengeID, memberID, categoryID uuid.UUID, exercises ExerciseSelection, now time.Time) *Participant {
	return &Participant{
		ID:           id,
		GymID:        gymID,
		ChallengeID:  challengeID,
		MemberID:     memberID,
		CategoryID:   categoryID,
		Version:      1,
		ExerciseLegs: strings.TrimSpace(exercises.Legs),
		ExercisePush: strings.TrimSpace(exercises.Push),
		ExercisePull: strings.TrimSpace(exercises.Pull),
		Status:       StatusRegistered,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

// ExerciseSelection groups the three pattern choices a participant makes
// at registration. Use cases pass this as a unit so the entity doesn't
// grow argument lists.
type ExerciseSelection struct {
	Legs string
	Push string
	Pull string
}

// MarkFeePaid records the inscription cash payment. Idempotent: paying
// twice doesn't bump version (and avoids accidentally overwriting the
// original paid-at timestamp).
func (p *Participant) MarkFeePaid(at time.Time) {
	if p.InscriptionFeePaid {
		return
	}
	p.InscriptionFeePaid = true
	p.InscriptionPaidAt = &at
	p.Version++
	p.UpdatedAt = at
}

// RefundFee marks the fee as refunded (kept paid=true for audit).
func (p *Participant) RefundFee(at time.Time) {
	if !p.InscriptionFeePaid || p.InscriptionRefundedAt != nil {
		return
	}
	p.InscriptionRefundedAt = &at
	p.Version++
	p.UpdatedAt = at
}

// Activate registered → active. Called after T₀ is captured (the use
// case decides the trigger; entity just enforces the transition).
func (p *Participant) Activate(now time.Time) error {
	if p.Status != StatusRegistered {
		return challengeErrors.ErrParticipantInactive
	}
	p.Status = StatusActive
	p.Version++
	p.UpdatedAt = now
	return nil
}

// Disqualify marks the participant out for failing attendance / safety
// rules. Idempotent (re-disqualifying doesn't overwrite the reason).
func (p *Participant) Disqualify(reason string, now time.Time) {
	if p.Status == StatusDisqualified {
		return
	}
	p.Status = StatusDisqualified
	p.DisqualificationReason = strings.TrimSpace(reason)
	p.DisqualifiedAt = &now
	p.Version++
	p.UpdatedAt = now
}

// Withdraw is the participant's own pull-out. Voluntary, no DQ reason.
func (p *Participant) Withdraw(now time.Time) {
	if p.Status == StatusWithdrew || p.Status == StatusCompleted {
		return
	}
	p.Status = StatusWithdrew
	p.Version++
	p.UpdatedAt = now
}

// Complete final-state mark, set at challenge close for participants
// who finished valid (both T₀ + T₁ captured, attendance ok).
func (p *Participant) Complete(now time.Time) {
	if p.Status == StatusCompleted {
		return
	}
	p.Status = StatusCompleted
	p.Version++
	p.UpdatedAt = now
}

// UpdateExercises lets the use case correct a participant's exercise
// selection before T₀ is captured. The use case checks for the
// "before T₀" precondition.
func (p *Participant) UpdateExercises(e ExerciseSelection, now time.Time) {
	p.ExerciseLegs = strings.TrimSpace(e.Legs)
	p.ExercisePush = strings.TrimSpace(e.Push)
	p.ExercisePull = strings.TrimSpace(e.Pull)
	p.Version++
	p.UpdatedAt = now
}
