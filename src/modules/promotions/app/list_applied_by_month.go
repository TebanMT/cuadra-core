package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// ListAppliedByMonthInput. Month en formato "2026-05". El mes se interpreta
// en la zona horaria del GYM: created_at es un instante, así que las cotas
// del mes se traducen a instantes locales — con las cotas UTC, una promo
// aplicada la noche del último día del mes caía en el resumen del mes
// siguiente, y el default de mes se corría desde las 6 PM de CDMX.
type ListAppliedByMonthInput struct {
	GymID uuid.UUID
	Month string
}

type ListAppliedByMonth struct {
	Repo promoRepo.AppliedPromotionRepository
	UoW  sharedDomain.UnitOfWork
	// Gyms (opcional) → mes local del gym. Nil = mes UTC (tests viejos).
	Gyms gymRepo.GymRepository
}

func NewListAppliedByMonth(repo promoRepo.AppliedPromotionRepository, uow sharedDomain.UnitOfWork) *ListAppliedByMonth {
	return &ListAppliedByMonth{Repo: repo, UoW: uow}
}

// WithGyms cablea el repo de gyms para anclar el mes a SU zona horaria.
func (uc *ListAppliedByMonth) WithGyms(g gymRepo.GymRepository) *ListAppliedByMonth {
	uc.Gyms = g
	return uc
}

func (uc *ListAppliedByMonth) Execute(ctx context.Context, in ListAppliedByMonthInput) ([]promoRepo.AppliedSummary, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	loc := time.UTC
	if uc.Gyms != nil {
		if g, gerr := uc.Gyms.GetByID(tx, in.GymID); gerr == nil && g != nil {
			loc = tz.LocationOrUTC(g.Timezone)
		}
	}
	month := in.Month
	if month == "" {
		// Default: el mes en curso del GYM (no el de UTC — el 31 en la
		// noche, UTC ya va en el mes siguiente y el resumen salía vacío).
		month = time.Now().In(loc).Format("2006-01")
	}
	parsed, err := time.Parse("2006-01", month)
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	// Cotas del mes como instantes en la zona del gym: [1º 00:00 local,
	// 1º del mes siguiente 00:00 local).
	start := time.Date(parsed.Year(), parsed.Month(), 1, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 1, 0)
	out, err := uc.Repo.SummaryByMonth(tx, in.GymID, start.UTC(), end.UTC())
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return out, nil
}
