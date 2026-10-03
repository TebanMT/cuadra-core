package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListMembersInput backs UC-014.
type ListMembersInput struct {
	GymID         uuid.UUID
	Search        string
	StatusFilter  string // "" / "active" / "expiring_soon" / "expired" / "inactive"
	PlanID        *uuid.UUID
	Sort          string // "name" / "expiry" / "created_at"
	SortAscending bool
	Page          int
	PageSize      int
}

// MemberListItem is the read model for a row in UC-014. Status is the
// *derived* AccessStatus (not the raw Member.Status) because that's what the
// UI shows in the colored chip.
type MemberListItem struct {
	Member            *memberDomain.Member
	CurrentMembership *membershipDomain.Membership
	AccessStatus      access.AccessStatus
}

type ListMembersOutput struct {
	Items    []MemberListItem
	Total    int
	Page     int
	PageSize int
}

type ListMembers struct {
	Gyms    gymRepo.GymRepository
	Members memRepo.MemberRepository
	UoW     sharedDomain.UnitOfWork
}

func NewListMembers(members memRepo.MemberRepository, uow sharedDomain.UnitOfWork) *ListMembers {
	return &ListMembers{Members: members, UoW: uow}
}

func (uc *ListMembers) WithGyms(gyms gymRepo.GymRepository) *ListMembers { uc.Gyms = gyms; return uc }

func (uc *ListMembers) Execute(ctx context.Context, in ListMembersInput) (*ListMembersOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := time.Now().UTC()
	if uc.Gyms != nil {
		gym, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		if gym != nil {
			if zone, err := time.LoadLocation(gym.Timezone); err == nil {
				now = now.In(zone)
			}
		}
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	page := in.Page
	if page < 1 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 25
	}
	rows, total, err := uc.Members.List(tx, memRepo.ListQuery{
		GymID:         in.GymID,
		Search:        in.Search,
		StatusFilter:  in.StatusFilter,
		PlanID:        in.PlanID,
		Sort:          in.Sort,
		SortAscending: in.SortAscending,
		Page:          page,
		PageSize:      pageSize,
		Today:         today,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	evaluator := access.New()
	items := make([]MemberListItem, 0, len(rows))
	for _, mw := range rows {
		items = append(items, MemberListItem{
			Member:            mw.Member,
			CurrentMembership: mw.CurrentMembership,
			AccessStatus:      evaluator.Evaluate(mw.Member, mw.CurrentMembership, today),
		})
	}
	return &ListMembersOutput{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}
