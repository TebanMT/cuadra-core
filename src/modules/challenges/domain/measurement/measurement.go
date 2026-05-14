// Package measurement — composition + strength snapshot at a single
// moment (T₀, T₁, or intermediate). Immutable in semantics: corrections
// create a successor row and mark the prior as superseded. The entity
// itself models both shapes: a fresh measurement, and the supersession
// link that ties two rows together.
package measurement

import (
	"strings"
	"time"

	"github.com/google/uuid"

	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/challenges/domain/scoring"
)

// Moment values match the postgres CHECK constraint exactly. The
// scoring formula consumes T₀ and T₁ exclusively; "intermediate" is
// observational (semana 10 check-in) and never enters the ranking.
const (
	MomentT0           = "t0"
	MomentT1           = "t1"
	MomentIntermediate = "intermediate"
)

// Physiological / protocol bounds — keep these aligned with the
// validation copy shown on the FE capture modal. Constants live here so
// the FE and BE references both point to the same authoritative file
// when copy needs to change.
const (
	BodyFatMinPct = 3.0
	BodyFatMaxPct = 60.0
	RepsMin       = 1
	RepsMax       = 15
)

type Measurement struct {
	ID              uuid.UUID
	GymID           uuid.UUID
	ParticipantID   uuid.UUID
	Version         int
	Moment          string
	MeasuredAt      time.Time
	BodyWeightKg    float64
	BodyFatPct      float64
	LegsWeightKg    float64
	LegsReps        int
	PushWeightKg    float64
	PushReps        int
	PullWeightKg    float64
	PullReps        int
	Notes           string
	CreatedByUserID uuid.UUID
	SupersededAt    *time.Time
	SupersededByID  *uuid.UUID
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       *time.Time
}

// Input is the validated payload the capture use case feeds NewMeasurement.
// Keeping the constructor signature small (3 vs 12 args) makes test
// fixtures readable.
type Input struct {
	Moment          string
	MeasuredAt      time.Time
	BodyWeightKg    float64
	BodyFatPct      float64
	LegsWeightKg    float64
	LegsReps        int
	PushWeightKg    float64
	PushReps        int
	PullWeightKg    float64
	PullReps        int
	Notes           string
	CreatedByUserID uuid.UUID
}

// NewMeasurement constructs a fresh measurement. Validates field ranges
// and the moment enum. The use case is still responsible for the
// "challenge window allows this moment now" precondition — that depends
// on the challenge state machine which this package can't see.
func NewMeasurement(id, gymID, participantID uuid.UUID, in Input, now time.Time) (*Measurement, error) {
	if err := ValidateMoment(in.Moment); err != nil {
		return nil, err
	}
	if in.BodyWeightKg <= 0 {
		return nil, challengeErrors.ErrWeightNonPositive
	}
	if in.BodyFatPct < BodyFatMinPct || in.BodyFatPct > BodyFatMaxPct {
		return nil, challengeErrors.ErrBodyFatOutOfRange
	}
	for _, w := range []float64{in.LegsWeightKg, in.PushWeightKg, in.PullWeightKg} {
		if w <= 0 {
			return nil, challengeErrors.ErrWeightNonPositive
		}
	}
	for _, r := range []int{in.LegsReps, in.PushReps, in.PullReps} {
		if r < RepsMin || r > RepsMax {
			return nil, challengeErrors.ErrRepsOutOfRange
		}
	}
	return &Measurement{
		ID:              id,
		GymID:           gymID,
		ParticipantID:   participantID,
		Version:         1,
		Moment:          in.Moment,
		MeasuredAt:      in.MeasuredAt,
		BodyWeightKg:    in.BodyWeightKg,
		BodyFatPct:      in.BodyFatPct,
		LegsWeightKg:    in.LegsWeightKg,
		LegsReps:        in.LegsReps,
		PushWeightKg:    in.PushWeightKg,
		PushReps:        in.PushReps,
		PullWeightKg:    in.PullWeightKg,
		PullReps:        in.PullReps,
		Notes:           strings.TrimSpace(in.Notes),
		CreatedByUserID: in.CreatedByUserID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// ValidateMoment reports whether m is one of the legal enum values.
func ValidateMoment(m string) error {
	switch m {
	case MomentT0, MomentT1, MomentIntermediate:
		return nil
	}
	return challengeErrors.ErrMomentInvalid
}

// IsActive reports whether this measurement still counts (i.e. has not
// been replaced by a correction). The ranking query MUST filter on this.
func (m *Measurement) IsActive() bool {
	return m.SupersededAt == nil && m.DeletedAt == nil
}

// MarkSuperseded points this measurement at its replacement. The
// supersession is a one-way link; the new row's ID goes here.
// Idempotent: calling twice leaves the first replacement in place.
func (m *Measurement) MarkSuperseded(replacementID uuid.UUID, now time.Time) {
	if m.SupersededAt != nil {
		return
	}
	m.SupersededAt = &now
	m.SupersededByID = &replacementID
	m.Version++
	m.UpdatedAt = now
}

// ToScoringInput converts the stored raw weight+reps into the scoring
// package's Measurement shape (1RMs already derived via Epley). Keeping
// the derivation out of the persistence layer lets us tweak Epley → some
// other formula in a future challenge edition without a data migration.
func (m *Measurement) ToScoringInput() scoring.Measurement {
	return scoring.Measurement{
		BodyWeight: m.BodyWeightKg,
		BodyFatPct: m.BodyFatPct,
		Legs1RM:    scoring.EstimateOneRepMax(m.LegsWeightKg, m.LegsReps),
		Push1RM:    scoring.EstimateOneRepMax(m.PushWeightKg, m.PushReps),
		Pull1RM:    scoring.EstimateOneRepMax(m.PullWeightKg, m.PullReps),
	}
}
