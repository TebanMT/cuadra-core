package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	caDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/contact_attempt"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// MarkContactedInput backs UC-035 step "Le hablé". Channel + Note are
// optional; the operator may just tap the button without filling the note.
type MarkContactedInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	MemberID    uuid.UUID
	Channel     *string
	Note        *string
}

type MarkContactedOutput struct {
	ContactAttemptID     uuid.UUID
	MemberID             uuid.UUID
	LastContactAttemptAt time.Time
}

type MarkContacted struct {
	Members  memRepo.MemberRepository
	Attempts memRepo.ContactAttemptRepository
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
}

func NewMarkContacted(members memRepo.MemberRepository, attempts memRepo.ContactAttemptRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *MarkContacted {
	return &MarkContacted{Members: members, Attempts: attempts, UoW: uow, Audit: recorder}
}

func (uc *MarkContacted) Execute(ctx context.Context, in MarkContactedInput) (*MarkContactedOutput, error) {
	now := time.Now().UTC()
	var out MarkContactedOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		m, err := uc.Members.GetByID(tx, in.MemberID)
		if err != nil {
			return err
		}
		if m.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
		}
		attempt, err := caDomain.New(uuid.New(), in.GymID, in.MemberID, in.ActorUserID,
			now, in.Channel, in.Note, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err := uc.Attempts.Create(tx, attempt); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		m.RegisterContactAttempt(now)
		updated, err := uc.Members.Update(tx, m)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "contact_attempts",
			EntityID:    attempt.ID,
			Action:      "create",
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"member_id":  in.MemberID.String(),
				"channel":    derefString(in.Channel),
				"has_note":   in.Note != nil && *in.Note != "",
				"attempt_at": attempt.AttemptAt.Format(time.RFC3339),
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = MarkContactedOutput{
			ContactAttemptID:     attempt.ID,
			MemberID:             updated.ID,
			LastContactAttemptAt: *updated.LastContactAttemptAt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
