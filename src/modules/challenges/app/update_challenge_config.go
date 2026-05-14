package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	challengeRepo "github.com/cuadra/cuadra-core/src/modules/challenges/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// UpdateChallengeConfig is UC-Reto-002. Wraps Challenge.ApplyConfig and
// gates it on "no measurements captured yet" — that's the rule that
// protects participants from a host changing the rules mid-event.
type UpdateChallengeConfig struct {
	Challenges   challengeRepo.ChallengeRepository
	Measurements challengeRepo.MeasurementRepository
	UoW          sharedDomain.UnitOfWork
	Audit        audit.Recorder
	NowFunc      func() time.Time
}

func NewUpdateChallengeConfig(
	challenges challengeRepo.ChallengeRepository,
	measurements challengeRepo.MeasurementRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *UpdateChallengeConfig {
	return &UpdateChallengeConfig{
		Challenges:   challenges,
		Measurements: measurements,
		UoW:          uow,
		Audit:        recorder,
		NowFunc:      func() time.Time { return time.Now().UTC() },
	}
}

type UpdateChallengeConfigInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	ChallengeID uuid.UUID
	Config      challengeDomain.ConfigUpdate
}

func (uc *UpdateChallengeConfig) Execute(ctx context.Context, in UpdateChallengeConfigInput) (*challengeDomain.Challenge, error) {
	now := uc.NowFunc()
	var result *challengeDomain.Challenge
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		c, err := uc.Challenges.GetByID(tx, in.ChallengeID)
		if err != nil {
			return err
		}
		if c.GymID != in.GymID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		// The hard rule: once any measurement exists, the rules are frozen.
		// Status-level locking inside ApplyConfig is the *softer* check
		// (draft/open_registration); this is the harder one.
		n, err := uc.Measurements.CountByChallenge(tx, c.ID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if n > 0 {
			return sharedDomain.NewBusinessError(challengeErrors.ErrConfigLocked, "")
		}
		if err := c.ApplyConfig(in.Config, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		updated, err := uc.Challenges.Update(tx, c)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		result = updated
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "challenges",
			EntityID:    c.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"version": updated.Version},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
