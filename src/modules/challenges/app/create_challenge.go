// Package app holds the challenges (retos) use cases. Each use case is a
// struct with constructor-style dependency injection — same pattern as
// the rest of Tinta. UoW.Command wraps writes so a failure half-way
// through (audit, validation, FK error) rolls back the whole operation.
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeRepo "github.com/cuadra/cuadra-core/src/modules/challenges/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CreateChallengeInput is the wire-agnostic payload. Pointer fields
// represent "use default" semantics — the handler maps an absent JSON
// field to a nil pointer here so the entity's default values apply.
type CreateChallengeInput struct {
	GymID                 uuid.UUID
	ActorUserID           uuid.UUID
	Name                  string
	Description           string
	StartsAt              time.Time
	MeasurementT0Deadline time.Time
	MeasurementT1Start    time.Time
	EndsAt                time.Time
	// Overrides — leave nil to keep the entity's defaults.
	InscriptionFeeCents   *int
	InscriptionRefundable *bool
	MinWeeklyAttendance   *int
	AttendanceGraceWeeks  *int
	StrengthCapPct        *float64
	TieMarginIR           *float64
	BFFloorMalePct        *float64
	BFFloorFemalePct      *float64
}

// CreateChallenge is UC-Reto-001. Creates a challenge in `draft` status.
// The aggregate has no categories yet — the use case for that (Session 2)
// will be AddCategory; opening registration is also a separate use case
// that enforces "at least one category" before allowing the transition.
type CreateChallenge struct {
	Challenges challengeRepo.ChallengeRepository
	UoW        sharedDomain.UnitOfWork
	Audit      audit.Recorder
	NowFunc    func() time.Time
}

func NewCreateChallenge(challenges challengeRepo.ChallengeRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreateChallenge {
	return &CreateChallenge{
		Challenges: challenges,
		UoW:        uow,
		Audit:      recorder,
		NowFunc:    func() time.Time { return time.Now().UTC() },
	}
}

func (uc *CreateChallenge) Execute(ctx context.Context, in CreateChallengeInput) (*challengeDomain.Challenge, error) {
	now := uc.NowFunc()
	id := uuid.New()

	c, err := challengeDomain.NewChallenge(
		id, in.GymID,
		in.Name, in.Description,
		in.StartsAt, in.MeasurementT0Deadline, in.MeasurementT1Start, in.EndsAt,
		now,
	)
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	// Apply optional overrides through the entity's config path so the
	// same validations apply (e.g. negative fee rejected).
	if err := c.ApplyConfig(challengeDomain.ConfigUpdate{
		InscriptionFeeCents:   in.InscriptionFeeCents,
		InscriptionRefundable: in.InscriptionRefundable,
		MinWeeklyAttendance:   in.MinWeeklyAttendance,
		AttendanceGraceWeeks:  in.AttendanceGraceWeeks,
		StrengthCapPct:        in.StrengthCapPct,
		TieMarginIR:           in.TieMarginIR,
		BFFloorMalePct:        in.BFFloorMalePct,
		BFFloorFemalePct:      in.BFFloorFemalePct,
	}, now); err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	// ApplyConfig bumped Version to 2; reset to 1 because the row hasn't
	// been written yet — config is part of the initial state, not an edit.
	c.Version = 1

	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		if _, err := uc.Challenges.Create(tx, c); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "challenges",
			EntityID:    c.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"name": c.Name, "starts_at": c.StartsAt},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}
