package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListAppliedByMonthInput. Month en formato "2026-05" (use case parsea
// y arma [start, end) en UTC).
type ListAppliedByMonthInput struct {
	GymID uuid.UUID
	Month string
}

type ListAppliedByMonth struct {
	Repo promoRepo.AppliedPromotionRepository
	UoW  sharedDomain.UnitOfWork
}

func NewListAppliedByMonth(repo promoRepo.AppliedPromotionRepository, uow sharedDomain.UnitOfWork) *ListAppliedByMonth {
	return &ListAppliedByMonth{Repo: repo, UoW: uow}
}

func (uc *ListAppliedByMonth) Execute(ctx context.Context, in ListAppliedByMonthInput) ([]promoRepo.AppliedSummary, error) {
	month := in.Month
	if month == "" {
		month = time.Now().UTC().Format("2006-01")
	}
	start, err := time.Parse("2006-01", month)
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	end := start.AddDate(0, 1, 0)
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out, err := uc.Repo.SummaryByMonth(tx, in.GymID, start, end)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return out, nil
}
