package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ToggleMemberStatusInput backs UC-016. Reason is optional but persisted in
// audit_log when present (DA-16.1).
type ToggleMemberStatusInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	MemberID    uuid.UUID
	NewStatus   string
	Reason      string
}

type ToggleMemberStatus struct {
	Members memRepo.MemberRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
}

func NewToggleMemberStatus(members memRepo.MemberRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *ToggleMemberStatus {
	return &ToggleMemberStatus{Members: members, UoW: uow, Audit: recorder}
}

func (uc *ToggleMemberStatus) Execute(ctx context.Context, in ToggleMemberStatusInput) (*memberDomain.Member, error) {
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
		previous := m.Status
		if err := m.ChangeStatus(in.NewStatus, now); err != nil {
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
			Action:      audit.ActionToggleActive,
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
