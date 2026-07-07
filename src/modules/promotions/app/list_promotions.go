package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

type ListPromotionsInput struct {
	GymID           uuid.UUID
	IncludeInactive bool
	AppliesTo       string // "membership", "sale", "" para sin filtro
	// CurrentlyValid — si non-nil, sólo las vigentes en el DÍA del gym que
	// contiene ese instante. El use case trunca al día calendario local
	// (tz del gym) antes de filtrar: valid_from/valid_until son DATE
	// (medianoche UTC) y compararlas contra un timestamp con horas excluía
	// a las promos durante TODO su último día de vigencia, además de
	// correr el día desde las 6 PM en CDMX.
	CurrentlyValid *time.Time
}

type ListPromotions struct {
	Repo promoRepo.PromotionRepository
	UoW  sharedDomain.UnitOfWork
	// Gyms (opcional) → tz para el día local. Nil = día UTC (tests viejos).
	Gyms gymRepo.GymRepository
}

func NewListPromotions(repo promoRepo.PromotionRepository, uow sharedDomain.UnitOfWork) *ListPromotions {
	return &ListPromotions{Repo: repo, UoW: uow}
}

// WithGyms cablea el repo de gyms para evaluar la vigencia sobre el día
// calendario del gym en SU zona horaria.
func (uc *ListPromotions) WithGyms(g gymRepo.GymRepository) *ListPromotions {
	uc.Gyms = g
	return uc
}

func (uc *ListPromotions) Execute(ctx context.Context, in ListPromotionsInput) ([]*promoDomain.Promotion, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	currentlyValid := in.CurrentlyValid
	if currentlyValid != nil {
		day := localPromoDay(tx, uc.Gyms, in.GymID, *currentlyValid)
		currentlyValid = &day
	}
	rows, err := uc.Repo.List(tx, promoRepo.ListFilter{
		GymID:           in.GymID,
		IncludeInactive: in.IncludeInactive,
		AppliesTo:       in.AppliesTo,
		CurrentlyValid:  currentlyValid,
	})
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return rows, nil
}

// localPromoDay — día calendario del gym (medianoche UTC) para el filtro
// de vigencia. Fail-open al día UTC si no hay repo/gym/tz.
func localPromoDay(
	tx sharedDomain.Transaction,
	gyms gymRepo.GymRepository,
	gymID uuid.UUID,
	now time.Time,
) time.Time {
	if gyms == nil {
		u := now.UTC()
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	}
	g, err := gyms.GetByID(tx, gymID)
	if err != nil || g == nil {
		u := now.UTC()
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	}
	return tz.LocalToday(g.Timezone, now)
}
