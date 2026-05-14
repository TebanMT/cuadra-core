// Package challenge holds the Challenge aggregate — the root entity for
// the "retos" bounded context. Owns lifecycle (status transitions) and
// configuration (cap, margin, attendance rules, dates).
//
// State machine:
//
//	draft ──open──> open_registration ──start──> running ──t1──> measuring_t1 ──close──> closed
//	  │                    │                        │                 │
//	  └────cancel──────────┴────────cancel──────────┴──────cancel─────┴─────> cancelled
//
// Mutations that change the formula's behavior (strength cap, tie margin,
// dates) are rejected once any measurement has been captured — that's
// enforced at the use-case layer because the entity doesn't have access
// to the measurement count.
package challenge

import (
	"strings"
	"time"

	"github.com/google/uuid"

	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
)

// Status constants — match the postgres CHECK constraint exactly. Adding
// a new value here requires a migration to widen the constraint.
const (
	StatusDraft            = "draft"
	StatusOpenRegistration = "open_registration"
	StatusRunning          = "running"
	StatusMeasuringT1      = "measuring_t1"
	StatusClosed           = "closed"
	StatusCancelled        = "cancelled"
)

// Sensible defaults for a "Reto 12"-style challenge. Use cases consume
// these when the caller omits the corresponding field.
const (
	DefaultStrengthCapPct     = 25.0
	DefaultTieMarginIR        = 5.0
	DefaultMinWeeklyAttend    = 3
	DefaultAttendanceGraceWks = 2
	DefaultBFFloorMalePct     = 6.0
	DefaultBFFloorFemalePct   = 14.0
)

// Challenge is the aggregate root. Mutations bump Version + UpdatedAt
// (sync uses Version for last-write-wins; UpdatedAt drives delta-pull).
type Challenge struct {
	ID                    uuid.UUID
	GymID                 uuid.UUID
	Version               int
	Name                  string
	Description           string
	StartsAt              time.Time
	MeasurementT0Deadline time.Time
	MeasurementT1Start    time.Time
	EndsAt                time.Time
	Status                string
	InscriptionFeeCents   int
	InscriptionRefundable bool
	MinWeeklyAttendance   int
	AttendanceGraceWeeks  int
	StrengthCapPct        float64
	TieMarginIR           float64
	BFFloorMalePct        float64
	BFFloorFemalePct      float64
	CreatedAt             time.Time
	UpdatedAt             time.Time
	DeletedAt             *time.Time
}

// NewChallenge constructs a draft challenge with defaults filled in.
// Validates name + date ordering at construction time so an invalid
// aggregate can never exist (use cases get a friendly error before any
// repo touches the DB).
func NewChallenge(
	id, gymID uuid.UUID,
	name, description string,
	startsAt, t0Deadline, t1Start, endsAt time.Time,
	now time.Time,
) (*Challenge, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, challengeErrors.ErrNameRequired
	}
	if err := validateDates(startsAt, t0Deadline, t1Start, endsAt); err != nil {
		return nil, err
	}
	return &Challenge{
		ID:                    id,
		GymID:                 gymID,
		Version:               1,
		Name:                  trimmed,
		Description:           strings.TrimSpace(description),
		StartsAt:              startsAt,
		MeasurementT0Deadline: t0Deadline,
		MeasurementT1Start:    t1Start,
		EndsAt:                endsAt,
		Status:                StatusDraft,
		InscriptionRefundable: true,
		MinWeeklyAttendance:   DefaultMinWeeklyAttend,
		AttendanceGraceWeeks:  DefaultAttendanceGraceWks,
		StrengthCapPct:        DefaultStrengthCapPct,
		TieMarginIR:           DefaultTieMarginIR,
		BFFloorMalePct:        DefaultBFFloorMalePct,
		BFFloorFemalePct:      DefaultBFFloorFemalePct,
		CreatedAt:             now,
		UpdatedAt:             now,
	}, nil
}

func validateDates(startsAt, t0Deadline, t1Start, endsAt time.Time) error {
	if !startsAt.Before(t0Deadline) {
		return challengeErrors.ErrInvalidDates
	}
	if t0Deadline.After(t1Start) {
		return challengeErrors.ErrInvalidDates
	}
	if !t1Start.Before(endsAt) {
		return challengeErrors.ErrInvalidDates
	}
	return nil
}

// ─── lifecycle ─────────────────────────────────────────────────────────────

// OpenRegistration draft → open_registration. Caller (use case) must
// first verify the challenge has at least one category.
func (c *Challenge) OpenRegistration(now time.Time) error {
	if c.Status != StatusDraft {
		return challengeErrors.ErrInvalidStatusTransition
	}
	c.Status = StatusOpenRegistration
	c.touch(now)
	return nil
}

// StartRunning open_registration → running. Locks new registrations.
func (c *Challenge) StartRunning(now time.Time) error {
	if c.Status != StatusOpenRegistration {
		return challengeErrors.ErrInvalidStatusTransition
	}
	c.Status = StatusRunning
	c.touch(now)
	return nil
}

// StartMeasuringT1 running → measuring_t1. Signals the T₁ capture window
// is open; use case independently checks `now >= MeasurementT1Start`
// before allowing actual measurement writes.
func (c *Challenge) StartMeasuringT1(now time.Time) error {
	if c.Status != StatusRunning {
		return challengeErrors.ErrInvalidStatusTransition
	}
	c.Status = StatusMeasuringT1
	c.touch(now)
	return nil
}

// Close measuring_t1 → closed. Triggers the final ranking calculation
// upstream.
func (c *Challenge) Close(now time.Time) error {
	if c.Status != StatusMeasuringT1 {
		return challengeErrors.ErrInvalidStatusTransition
	}
	c.Status = StatusClosed
	c.touch(now)
	return nil
}

// Cancel allows aborting from any active status. Closed challenges
// cannot be cancelled (they're already final).
func (c *Challenge) Cancel(now time.Time) error {
	switch c.Status {
	case StatusClosed, StatusCancelled:
		return challengeErrors.ErrInvalidStatusTransition
	}
	c.Status = StatusCancelled
	c.touch(now)
	return nil
}

// ─── config edits ──────────────────────────────────────────────────────────

// ConfigUpdate is the partial-update payload accepted by ApplyConfig.
// Pointer fields = "leave unchanged when nil". This shape mirrors what
// the PATCH handler accepts.
type ConfigUpdate struct {
	Name                  *string
	Description           *string
	StartsAt              *time.Time
	MeasurementT0Deadline *time.Time
	MeasurementT1Start    *time.Time
	EndsAt                *time.Time
	InscriptionFeeCents   *int
	InscriptionRefundable *bool
	MinWeeklyAttendance   *int
	AttendanceGraceWeeks  *int
	StrengthCapPct        *float64
	TieMarginIR           *float64
	BFFloorMalePct        *float64
	BFFloorFemalePct      *float64
}

// ApplyConfig mutates editable fields. The caller (use case) is
// responsible for the "no measurements captured yet" check — this method
// only enforces that the challenge is in a status where config edits
// make sense (draft or open_registration).
func (c *Challenge) ApplyConfig(u ConfigUpdate, now time.Time) error {
	if c.Status != StatusDraft && c.Status != StatusOpenRegistration {
		return challengeErrors.ErrConfigLocked
	}

	// Stage the new date set so we validate before mutating.
	startsAt := c.StartsAt
	t0 := c.MeasurementT0Deadline
	t1 := c.MeasurementT1Start
	endsAt := c.EndsAt
	if u.StartsAt != nil {
		startsAt = *u.StartsAt
	}
	if u.MeasurementT0Deadline != nil {
		t0 = *u.MeasurementT0Deadline
	}
	if u.MeasurementT1Start != nil {
		t1 = *u.MeasurementT1Start
	}
	if u.EndsAt != nil {
		endsAt = *u.EndsAt
	}
	if err := validateDates(startsAt, t0, t1, endsAt); err != nil {
		return err
	}

	if u.Name != nil {
		trimmed := strings.TrimSpace(*u.Name)
		if trimmed == "" {
			return challengeErrors.ErrNameRequired
		}
		c.Name = trimmed
	}
	if u.Description != nil {
		c.Description = strings.TrimSpace(*u.Description)
	}
	c.StartsAt = startsAt
	c.MeasurementT0Deadline = t0
	c.MeasurementT1Start = t1
	c.EndsAt = endsAt
	if u.InscriptionFeeCents != nil {
		if *u.InscriptionFeeCents < 0 {
			return challengeErrors.ErrInvalidConfig
		}
		c.InscriptionFeeCents = *u.InscriptionFeeCents
	}
	if u.InscriptionRefundable != nil {
		c.InscriptionRefundable = *u.InscriptionRefundable
	}
	if u.MinWeeklyAttendance != nil {
		if *u.MinWeeklyAttendance < 0 || *u.MinWeeklyAttendance > 7 {
			return challengeErrors.ErrInvalidConfig
		}
		c.MinWeeklyAttendance = *u.MinWeeklyAttendance
	}
	if u.AttendanceGraceWeeks != nil {
		if *u.AttendanceGraceWeeks < 0 {
			return challengeErrors.ErrInvalidConfig
		}
		c.AttendanceGraceWeeks = *u.AttendanceGraceWeeks
	}
	if u.StrengthCapPct != nil {
		if *u.StrengthCapPct < 0 {
			return challengeErrors.ErrInvalidConfig
		}
		c.StrengthCapPct = *u.StrengthCapPct
	}
	if u.TieMarginIR != nil {
		if *u.TieMarginIR < 0 {
			return challengeErrors.ErrInvalidConfig
		}
		c.TieMarginIR = *u.TieMarginIR
	}
	if u.BFFloorMalePct != nil {
		if *u.BFFloorMalePct < 0 || *u.BFFloorMalePct > 30 {
			return challengeErrors.ErrInvalidConfig
		}
		c.BFFloorMalePct = *u.BFFloorMalePct
	}
	if u.BFFloorFemalePct != nil {
		if *u.BFFloorFemalePct < 0 || *u.BFFloorFemalePct > 40 {
			return challengeErrors.ErrInvalidConfig
		}
		c.BFFloorFemalePct = *u.BFFloorFemalePct
	}
	c.touch(now)
	return nil
}

// ─── window predicates (UI + use-case checks) ─────────────────────────────

// AllowsT0Capture reports whether the moment 't0' is within the window
// the use case will accept (status ≥ open_registration, before deadline).
func (c *Challenge) AllowsT0Capture(now time.Time) bool {
	if c.Status != StatusOpenRegistration && c.Status != StatusRunning {
		return false
	}
	return !now.After(c.MeasurementT0Deadline)
}

// AllowsT1Capture reports whether moment 't1' captures are permitted —
// status must be measuring_t1 or closed-ish and now must be in the T₁
// window.
func (c *Challenge) AllowsT1Capture(now time.Time) bool {
	if c.Status != StatusMeasuringT1 && c.Status != StatusRunning {
		return false
	}
	if now.Before(c.MeasurementT1Start) {
		return false
	}
	return !now.After(c.EndsAt)
}

func (c *Challenge) touch(now time.Time) {
	c.Version++
	c.UpdatedAt = now
}
