package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// DeactivateMembershipTypeInput is the input for the soft-delete path
// (DA-11.2). Hard delete doesn't exist by design.
type DeactivateMembershipTypeInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	TypeID      uuid.UUID
}

type DeactivateMembershipType struct {
	Repo  memRepo.MembershipTypeRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewDeactivateMembershipType(repo memRepo.MembershipTypeRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *DeactivateMembershipType {
	return &DeactivateMembershipType{Repo: repo, UoW: uow, Audit: recorder}
}

func (uc *DeactivateMembershipType) Execute(ctx context.Context, in DeactivateMembershipTypeInput) (*mtDomain.MembershipType, error) {
	now := time.Now().UTC()
	var out *mtDomain.MembershipType
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		mt, err := uc.Repo.GetByID(tx, in.TypeID)
		if err != nil {
			return err
		}
		if mt.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeNotFound, "")
		}
		mt.Deactivate(now)
		updated, err := uc.Repo.Update(tx, mt)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "membership_types",
			EntityID:    updated.ID,
			Action:      audit.ActionToggleActive,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"active": false},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = updated
		return nil
	})
	return out, err
}
