package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ListPromotionsInput struct {
	GymID           uuid.UUID
	IncludeInactive bool
	AppliesTo       string     // "membership", "sale", "" para sin filtro
	CurrentlyValid  *time.Time // si non-nil, sólo las vigentes en esa fecha
}

type ListPromotions struct {
	Repo promoRepo.PromotionRepository
	UoW  sharedDomain.UnitOfWork
}

func NewListPromotions(repo promoRepo.PromotionRepository, uow sharedDomain.UnitOfWork) *ListPromotions {
	return &ListPromotions{Repo: repo, UoW: uow}
}

func (uc *ListPromotions) Execute(ctx context.Context, in ListPromotionsInput) ([]*promoDomain.Promotion, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	rows, err := uc.Repo.List(tx, promoRepo.ListFilter{
		GymID:           in.GymID,
		IncludeInactive: in.IncludeInactive,
		AppliesTo:       in.AppliesTo,
		CurrentlyValid:  in.CurrentlyValid,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return rows, nil
}
