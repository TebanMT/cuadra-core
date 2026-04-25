package app

import (
	"context"

	"github.com/google/uuid"

	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ListMembershipTypesInput struct {
	GymID           uuid.UUID
	IncludeInactive bool
}

type ListMembershipTypes struct {
	Repo memRepo.MembershipTypeRepository
	UoW  sharedDomain.UnitOfWork
}

func NewListMembershipTypes(repo memRepo.MembershipTypeRepository, uow sharedDomain.UnitOfWork) *ListMembershipTypes {
	return &ListMembershipTypes{Repo: repo, UoW: uow}
}

func (uc *ListMembershipTypes) Execute(ctx context.Context, in ListMembershipTypesInput) ([]*mtDomain.MembershipType, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out, err := uc.Repo.ListByGym(tx, in.GymID, in.IncludeInactive)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return out, nil
}
