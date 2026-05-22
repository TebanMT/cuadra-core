package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// UpdateMemberInput backs UC-013. nil = "no change".
type UpdateMemberInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	MemberID    uuid.UUID
	FullName    *string
	Phone       *string
	Email       *string
	Birthdate   *time.Time
	PhotoURL    *string
	Notes       *string
	// Gender — nil = no change (consistente con el resto del struct). El
	// valor "" limpia la captura (vuelve a NULL). Cualquier valor válido se
	// persiste tras ValidateGender.
	Gender *string
}

type UpdateMember struct {
	Members memRepo.MemberRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
}

func NewUpdateMember(members memRepo.MemberRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateMember {
	return &UpdateMember{Members: members, UoW: uow, Audit: recorder}
}

func (uc *UpdateMember) Execute(ctx context.Context, in UpdateMemberInput) (*memberDomain.Member, error) {
	now := time.Now().UTC()
	var out *memberDomain.Member
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		m, err := uc.Members.GetByID(tx, in.MemberID)
		if err != nil {
			return err
		}
		if m.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
		}
		// Phone uniqueness — only relevant if it actually changed.
		if in.Phone != nil {
			normalized, err := normalizeAndValidatePhone(*in.Phone)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if normalized != m.Phone {
				exists, err := uc.Members.ExistsByGymAndPhone(tx, in.GymID, normalized)
				if err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if exists {
					return sharedDomain.NewBusinessError(memErrors.ErrPhoneAlreadyExists, "")
				}
			}
			// Substitute the normalized version into the update so the domain
			// also runs it through ValidatePhone (idempotent).
			normCopy := normalized
			in.Phone = &normCopy
		}
		before := map[string]any{"full_name": m.FullName, "phone": m.Phone, "email": m.Email}
		if err := m.ApplyProfileUpdate(memberDomain.ProfileUpdate{
			FullName:  in.FullName,
			Phone:     in.Phone,
			Email:     in.Email,
			Birthdate: in.Birthdate,
			PhotoURL:  in.PhotoURL,
			Notes:     in.Notes,
			Gender:    in.Gender,
		}, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		updated, err := uc.Members.Update(tx, m)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "members",
			EntityID:    updated.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"before": before,
				"after":  map[string]any{"full_name": updated.FullName, "phone": updated.Phone, "email": updated.Email},
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = updated
		return nil
	})
	return out, err
}
