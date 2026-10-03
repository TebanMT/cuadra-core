package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// MarkLostInput backs UC-035 "Marcar perdido". Reason is optional; when
// present we audit it. DA-35.2 — "lost" is a terminal state; reactivation is
// UC-016 (ToggleMemberStatus).
//
// We could fold this into ToggleMemberStatus, but a dedicated use case keeps
// the audit action ("mark_lost") distinct from the generic toggle, which
// matters when the owner asks "who marked X as perdido and when".
type MarkLostInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	MemberID    uuid.UUID
	Reason      string
}

type MarkLost struct {
	Members memRepo.MemberRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
	Gyms    gymRepo.GymRepository
	Now     func() time.Time
}

func NewMarkLost(members memRepo.MemberRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *MarkLost {
	return &MarkLost{Members: members, UoW: uow, Audit: recorder, Now: time.Now}
}

func (uc *MarkLost) WithGyms(gyms gymRepo.GymRepository) *MarkLost {
	uc.Gyms = gyms
	return uc
}

func (uc *MarkLost) Execute(ctx context.Context, in MarkLostInput) (*memberDomain.Member, error) {
	now := uc.Now().UTC()
	var out *memberDomain.Member
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		mw, err := uc.Members.GetWithCurrentMembership(tx, in.GymID, in.MemberID)
		if err != nil {
			return err
		}
		m := mw.Member
		if m.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
		}
		if m.Status == memberDomain.StatusLost {
			return sharedDomain.NewBusinessError(memErrors.ErrMemberAlreadyLost, "")
		}
		today := tz.LocalToday("", now)
		if uc.Gyms != nil {
			gym, gymErr := uc.Gyms.GetByID(tx, in.GymID)
			if gymErr != nil {
				return sharedDomain.NewUnexpectedError(gymErr)
			}
			today = tz.LocalToday(gym.Timezone, now)
		}
		if current := mw.CurrentMembership; current != nil &&
			(current.ExpiryDate == nil || !current.ExpiryDate.Before(today)) {
			return sharedDomain.NewBusinessError(memErrors.ErrMemberStillCovered, "")
		}
		previous := m.Status
		if err := m.ChangeStatus(memberDomain.StatusLost, now); err != nil {
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
			Action:      "mark_lost",
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"before": previous,
				"after":  updated.Status,
				"reason": strings.TrimSpace(in.Reason),
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
