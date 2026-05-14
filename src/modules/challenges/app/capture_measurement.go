package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	challengeRepo "github.com/cuadra/cuadra-core/src/modules/challenges/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CaptureMeasurement is UC-Reto-005 — the highest-stakes write in the
// module. Each capture either creates a fresh measurement OR replaces a
// prior one via supersession; both paths run inside one UoW.Command so a
// half-completed correction is impossible.
//
// Order of operations (matters for ID linkage):
//
//  1. Validate moment + windows.
//  2. Find existing active measurement for (participant, moment).
//  3. Create the NEW row first — gets its ID assigned.
//  4. If an old row existed, Supersede(prior, new.ID, now) inside the same tx.
//  5. If moment=t0 and participant.status=registered, Activate() + Update.
//
// Doing Supersede AFTER Create lets the new row's ID be the
// `superseded_by_id` value — the audit trail "where did this row go"
// always points forward in time.
type CaptureMeasurement struct {
	Challenges   challengeRepo.ChallengeRepository
	Participants challengeRepo.ParticipantRepository
	Measurements challengeRepo.MeasurementRepository
	UoW          sharedDomain.UnitOfWork
	Audit        audit.Recorder
	NowFunc      func() time.Time
}

func NewCaptureMeasurement(
	challenges challengeRepo.ChallengeRepository,
	participants challengeRepo.ParticipantRepository,
	measurements challengeRepo.MeasurementRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *CaptureMeasurement {
	return &CaptureMeasurement{
		Challenges: challenges, Participants: participants, Measurements: measurements,
		UoW: uow, Audit: recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type CaptureMeasurementInput struct {
	GymID         uuid.UUID
	ActorUserID   uuid.UUID
	ChallengeID   uuid.UUID
	ParticipantID uuid.UUID
	Input         measurementDomain.Input
}

type CaptureMeasurementOutput struct {
	Measurement       *measurementDomain.Measurement
	SupersededPriorID *uuid.UUID
	ParticipantStatus string
}

func (uc *CaptureMeasurement) Execute(ctx context.Context, in CaptureMeasurementInput) (*CaptureMeasurementOutput, error) {
	now := uc.NowFunc()

	if err := measurementDomain.ValidateMoment(in.Input.Moment); err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}

	out := &CaptureMeasurementOutput{}
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		ch, err := uc.Challenges.GetByID(tx, in.ChallengeID)
		if err != nil {
			return err
		}
		if ch.GymID != in.GymID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		p, err := uc.Participants.GetByID(tx, in.ParticipantID)
		if err != nil {
			return err
		}
		if p.GymID != in.GymID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		if p.ChallengeID != ch.ID {
			return sharedDomain.NewValidationError(challengeErrors.ErrParticipantWrongChallenge)
		}

		// Window check: the challenge state machine decides whether the
		// moment is capturable RIGHT NOW. Intermediate captures use the T₀
		// (post-start) window for simplicity — they're observational only.
		switch in.Input.Moment {
		case measurementDomain.MomentT0, measurementDomain.MomentIntermediate:
			if !ch.AllowsT0Capture(now) {
				return sharedDomain.NewBusinessError(challengeErrors.ErrMeasurementOutOfWindow, "")
			}
		case measurementDomain.MomentT1:
			if !ch.AllowsT1Capture(now) {
				return sharedDomain.NewBusinessError(challengeErrors.ErrMeasurementOutOfWindow, "")
			}
		}

		// Default the operator on the measurement to the caller when the
		// FE didn't pass one — saves the handler from caring about it.
		captureInput := in.Input
		if captureInput.CreatedByUserID == uuid.Nil {
			captureInput.CreatedByUserID = in.ActorUserID
		}
		newM, err := measurementDomain.NewMeasurement(uuid.New(), in.GymID, p.ID, captureInput, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}

		// Look up the active measurement for the same moment BEFORE writing
		// the new one — supersession is a pair-up between two rows.
		prior, hasPrior, err := uc.Measurements.GetActiveByMoment(tx, p.ID, captureInput.Moment)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		saved, err := uc.Measurements.Create(tx, newM)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		out.Measurement = saved

		if hasPrior {
			if err := uc.Measurements.Supersede(tx, prior.ID, saved.ID, now); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			priorID := prior.ID
			out.SupersededPriorID = &priorID
		}

		// Capturing T₀ promotes a registered participant to active.
		// Intermediate / T₁ don't touch status here — that's the state
		// machine's job (StartMeasuringT1, Close, etc).
		if captureInput.Moment == measurementDomain.MomentT0 && p.Status == participantDomain.StatusRegistered {
			if err := p.Activate(now); err != nil {
				return sharedDomain.NewBusinessError(err, "")
			}
			if _, err := uc.Participants.Update(tx, p); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}
		out.ParticipantStatus = p.Status

		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "challenge_measurements",
			EntityID:    saved.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"participant_id": p.ID,
				"moment":         captureInput.Moment,
				"superseded":     hasPrior,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListMeasurements is the audit / display read for a single participant —
// returns every row (active + superseded), newest first. The capture modal
// uses this to render the "ya existe una medición" warning.
type ListMeasurements struct {
	Challenges   challengeRepo.ChallengeRepository
	Participants challengeRepo.ParticipantRepository
	Measurements challengeRepo.MeasurementRepository
	UoW          sharedDomain.UnitOfWork
}

func NewListMeasurements(
	challenges challengeRepo.ChallengeRepository,
	participants challengeRepo.ParticipantRepository,
	measurements challengeRepo.MeasurementRepository,
	uow sharedDomain.UnitOfWork,
) *ListMeasurements {
	return &ListMeasurements{
		Challenges: challenges, Participants: participants, Measurements: measurements,
		UoW: uow,
	}
}

type ListMeasurementsInput struct {
	GymID         uuid.UUID
	ChallengeID   uuid.UUID
	ParticipantID uuid.UUID
}

func (uc *ListMeasurements) Execute(ctx context.Context, in ListMeasurementsInput) ([]*measurementDomain.Measurement, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	p, err := uc.Participants.GetByID(tx, in.ParticipantID)
	if err != nil {
		return nil, err
	}
	if p.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
	}
	if p.ChallengeID != in.ChallengeID {
		return nil, sharedDomain.NewValidationError(challengeErrors.ErrParticipantWrongChallenge)
	}
	out, err := uc.Measurements.ListByParticipant(tx, p.ID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return out, nil
}
