package app

import (
	"context"

	"github.com/google/uuid"

	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeRepo "github.com/cuadra/cuadra-core/src/modules/challenges/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListChallenges returns the gym's challenges newest-first. Pagination is
// done in-memory because a gym is highly unlikely to have more than a
// handful of retos in flight at once — the SQL repo already orders, so we
// just slice the page out.
type ListChallenges struct {
	Challenges challengeRepo.ChallengeRepository
	UoW        sharedDomain.UnitOfWork
}

func NewListChallenges(challenges challengeRepo.ChallengeRepository, uow sharedDomain.UnitOfWork) *ListChallenges {
	return &ListChallenges{Challenges: challenges, UoW: uow}
}

type ListChallengesInput struct {
	GymID    uuid.UUID
	Status   string // "" → all
	Page     int    // 1-based; defaults to 1
	PageSize int    // defaults to 20, capped at 100
}

type ListChallengesOutput struct {
	Items    []*challengeDomain.Challenge
	Total    int
	Page     int
	PageSize int
}

func (uc *ListChallenges) Execute(ctx context.Context, in ListChallengesInput) (*ListChallengesOutput, error) {
	page, size := normalizePage(in.Page, in.PageSize)

	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	all, err := uc.Challenges.ListByGym(tx, in.GymID, in.Status)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	total := len(all)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	return &ListChallengesOutput{
		Items:    all[start:end],
		Total:    total,
		Page:     page,
		PageSize: size,
	}, nil
}

func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
