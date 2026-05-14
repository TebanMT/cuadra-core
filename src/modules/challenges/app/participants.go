package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	challengeRepo "github.com/cuadra/cuadra-core/src/modules/challenges/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ─── AddParticipant ────────────────────────────────────────────────────────

// AddParticipant is UC-Reto-004. Inscribes a member in the challenge under
// a chosen category. Defaults the three exercise picks to
// Sentadilla / Press de Banca / Peso Muerto when the caller leaves them blank.
type AddParticipant struct {
	Challenges   challengeRepo.ChallengeRepository
	Categories   challengeRepo.CategoryRepository
	Participants challengeRepo.ParticipantRepository
	UoW          sharedDomain.UnitOfWork
	Audit        audit.Recorder
	NowFunc      func() time.Time
}

func NewAddParticipant(
	challenges challengeRepo.ChallengeRepository,
	categories challengeRepo.CategoryRepository,
	participants challengeRepo.ParticipantRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *AddParticipant {
	return &AddParticipant{
		Challenges: challenges, Categories: categories, Participants: participants,
		UoW: uow, Audit: recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type AddParticipantInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	ChallengeID uuid.UUID
	MemberID    uuid.UUID
	CategoryID  uuid.UUID
	Exercises   participantDomain.ExerciseSelection
}

func (uc *AddParticipant) Execute(ctx context.Context, in AddParticipantInput) (*participantDomain.Participant, error) {
	now := uc.NowFunc()
	var result *participantDomain.Participant
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		ch, err := uc.Challenges.GetByID(tx, in.ChallengeID)
		if err != nil {
			return err
		}
		if ch.GymID != in.GymID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		if ch.Status != challengeDomain.StatusOpenRegistration {
			return sharedDomain.NewBusinessError(challengeErrors.ErrChallengeNotRegistering, "")
		}
		cat, err := uc.Categories.GetByID(tx, in.CategoryID)
		if err != nil {
			return err
		}
		if cat.ChallengeID != ch.ID {
			return sharedDomain.NewValidationError(challengeErrors.ErrCategoryMismatch)
		}
		exists, err := uc.Participants.ExistsByMember(tx, ch.ID, in.MemberID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if exists {
			return sharedDomain.NewBusinessError(challengeErrors.ErrAlreadyParticipating, "")
		}

		ex := defaultExercises(in.Exercises)
		p := participantDomain.NewParticipant(uuid.New(), in.GymID, ch.ID, in.MemberID, cat.ID, ex, now)
		saved, err := uc.Participants.Create(tx, p)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		result = saved
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "challenge_participants",
			EntityID:    saved.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"challenge_id": ch.ID,
				"member_id":    in.MemberID,
				"category_id":  cat.ID,
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
	return result, nil
}

// defaultExercises substitutes the canonical Sentadilla / Press de Banca /
// Peso Muerto picks when the caller leaves any slot blank. Keeps the FE
// flow trivial (one-tap inscribir) without losing the option to pick
// alternatives (prensa / press_pecho_maquina / jalon_polea).
func defaultExercises(e participantDomain.ExerciseSelection) participantDomain.ExerciseSelection {
	if strings.TrimSpace(e.Legs) == "" {
		e.Legs = participantDomain.ExerciseLegsSquat
	}
	if strings.TrimSpace(e.Push) == "" {
		e.Push = participantDomain.ExercisePushBenchPress
	}
	if strings.TrimSpace(e.Pull) == "" {
		e.Pull = participantDomain.ExercisePullDeadlift
	}
	return e
}

// ─── UpdateParticipant ─────────────────────────────────────────────────────

// UpdateParticipant lets the operator correct the exercise selection or
// flip status (mark fee paid, withdraw, etc.). Exercise edits are blocked
// once any measurement has been captured — the 1RMs are computed from the
// selected lift, so swapping the lift mid-event would invalidate the score.
type UpdateParticipant struct {
	Participants challengeRepo.ParticipantRepository
	UoW          sharedDomain.UnitOfWork
	Audit        audit.Recorder
	NowFunc      func() time.Time
}

func NewUpdateParticipant(
	participants challengeRepo.ParticipantRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *UpdateParticipant {
	return &UpdateParticipant{
		Participants: participants, UoW: uow, Audit: recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type UpdateParticipantInput struct {
	GymID         uuid.UUID
	ActorUserID   uuid.UUID
	ChallengeID   uuid.UUID
	ParticipantID uuid.UUID
	Exercises     *participantDomain.ExerciseSelection
	MarkFeePaid   bool
	Withdraw      bool
}

func (uc *UpdateParticipant) Execute(ctx context.Context, in UpdateParticipantInput) (*participantDomain.Participant, error) {
	now := uc.NowFunc()
	var result *participantDomain.Participant
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		p, err := uc.Participants.GetByID(tx, in.ParticipantID)
		if err != nil {
			return err
		}
		if p.GymID != in.GymID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		if p.ChallengeID != in.ChallengeID {
			return sharedDomain.NewValidationError(challengeErrors.ErrParticipantWrongChallenge)
		}
		if in.Exercises != nil {
			has, err := uc.Participants.HasAnyMeasurement(tx, p.ID)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if has {
				return sharedDomain.NewBusinessError(challengeErrors.ErrParticipantHasMeasurements, "")
			}
			p.UpdateExercises(*in.Exercises, now)
		}
		if in.MarkFeePaid {
			p.MarkFeePaid(now)
		}
		if in.Withdraw {
			p.Withdraw(now)
		}
		saved, err := uc.Participants.Update(tx, p)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		result = saved
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "challenge_participants",
			EntityID:    saved.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"version": saved.Version, "status": saved.Status},
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

// ─── RemoveParticipant ─────────────────────────────────────────────────────

// RemoveParticipant soft-deletes a participant who had no measurements yet
// (typically a wrong-click during inscripción). For participants who DID
// measure, the operator should use UpdateParticipant + Withdraw instead so
// the audit trail stays intact.
type RemoveParticipant struct {
	Participants challengeRepo.ParticipantRepository
	UoW          sharedDomain.UnitOfWork
	Audit        audit.Recorder
	NowFunc      func() time.Time
}

func NewRemoveParticipant(
	participants challengeRepo.ParticipantRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *RemoveParticipant {
	return &RemoveParticipant{
		Participants: participants, UoW: uow, Audit: recorder,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

type RemoveParticipantInput struct {
	GymID         uuid.UUID
	ActorUserID   uuid.UUID
	ChallengeID   uuid.UUID
	ParticipantID uuid.UUID
}

func (uc *RemoveParticipant) Execute(ctx context.Context, in RemoveParticipantInput) error {
	now := uc.NowFunc()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		p, err := uc.Participants.GetByID(tx, in.ParticipantID)
		if err != nil {
			return err
		}
		if p.GymID != in.GymID {
			return sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
		}
		if p.ChallengeID != in.ChallengeID {
			return sharedDomain.NewValidationError(challengeErrors.ErrParticipantWrongChallenge)
		}
		has, err := uc.Participants.HasAnyMeasurement(tx, p.ID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if has {
			return sharedDomain.NewBusinessError(challengeErrors.ErrParticipantHasMeasurements, "")
		}
		if err := uc.Participants.SoftDelete(tx, p.ID); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "challenge_participants",
			EntityID:    p.ID,
			Action:      audit.ActionDelete,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"member_id": p.MemberID},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
}

// ─── ListParticipants ──────────────────────────────────────────────────────

type ListParticipants struct {
	Challenges   challengeRepo.ChallengeRepository
	Participants challengeRepo.ParticipantRepository
	UoW          sharedDomain.UnitOfWork
}

func NewListParticipants(
	challenges challengeRepo.ChallengeRepository,
	participants challengeRepo.ParticipantRepository,
	uow sharedDomain.UnitOfWork,
) *ListParticipants {
	return &ListParticipants{Challenges: challenges, Participants: participants, UoW: uow}
}

type ListParticipantsInput struct {
	GymID       uuid.UUID
	ChallengeID uuid.UUID
	Status      string
	CategoryID  *uuid.UUID
}

func (uc *ListParticipants) Execute(ctx context.Context, in ListParticipantsInput) ([]*participantDomain.Participant, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	ch, err := uc.Challenges.GetByID(tx, in.ChallengeID)
	if err != nil {
		return nil, err
	}
	if ch.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrCrossGym, "")
	}
	out, err := uc.Participants.ListByChallenge(tx, ch.ID, in.Status, in.CategoryID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return out, nil
}
