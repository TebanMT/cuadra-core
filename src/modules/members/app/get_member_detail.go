package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type GetMemberDetailInput struct {
	GymID    uuid.UUID
	MemberID uuid.UUID
}

type GetMemberDetailOutput struct {
	Member            *memberDomain.Member
	CurrentMembership *membershipDomain.Membership
	AccessStatus      access.AccessStatus
	HasFingerprint    bool
}

type GetMemberDetail struct {
	Members      memRepo.MemberRepository
	Fingerprints memRepo.FingerprintRepository
	UoW          sharedDomain.UnitOfWork
}

func NewGetMemberDetail(members memRepo.MemberRepository, fingerprints memRepo.FingerprintRepository, uow sharedDomain.UnitOfWork) *GetMemberDetail {
	return &GetMemberDetail{Members: members, Fingerprints: fingerprints, UoW: uow}
}

func (uc *GetMemberDetail) Execute(ctx context.Context, in GetMemberDetailInput) (*GetMemberDetailOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	mw, err := uc.Members.GetWithCurrentMembership(tx, in.GymID, in.MemberID)
	if err != nil {
		return nil, err
	}
	if mw.Member == nil {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMemberNotFound, "")
	}

	fps, err := uc.Fingerprints.ListByMember(tx, in.MemberID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	st := access.New().Evaluate(mw.Member, mw.CurrentMembership, today)
	return &GetMemberDetailOutput{
		Member:            mw.Member,
		CurrentMembership: mw.CurrentMembership,
		AccessStatus:      st,
		HasFingerprint:    len(fps) > 0,
	}, nil
}
