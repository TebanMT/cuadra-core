package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	promoErrors "github.com/cuadra/cuadra-core/src/modules/promotions/domain/errors"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// GetPromotionByCodeInput. Code es case-insensitive (lo normalizamos
// internamente).
type GetPromotionByCodeInput struct {
	GymID uuid.UUID
	Code  string
	// Today es opcional. Cuando viene, valida vigencia + límite de uso
	// total (no per-member porque sin member_id no hay a quién atribuir).
	Today *time.Time
}

type GetPromotionByCode struct {
	Promotions promoRepo.PromotionRepository
	Applied    promoRepo.AppliedPromotionRepository
	UoW        sharedDomain.UnitOfWork
	// Gyms (opcional) → la vigencia se evalúa sobre el día LOCAL del gym
	// (ver localPromoDay). Nil = día UTC (tests viejos).
	Gyms gymRepo.GymRepository
}

func NewGetPromotionByCode(p promoRepo.PromotionRepository, ap promoRepo.AppliedPromotionRepository, uow sharedDomain.UnitOfWork) *GetPromotionByCode {
	return &GetPromotionByCode{Promotions: p, Applied: ap, UoW: uow}
}

// WithGyms cablea el repo de gyms para evaluar la vigencia sobre el día
// calendario del gym en SU zona horaria.
func (uc *GetPromotionByCode) WithGyms(g gymRepo.GymRepository) *GetPromotionByCode {
	uc.Gyms = g
	return uc
}

func (uc *GetPromotionByCode) Execute(ctx context.Context, in GetPromotionByCodeInput) (*promoDomain.Promotion, error) {
	codeLower := strings.ToLower(strings.TrimSpace(in.Code))
	if codeLower == "" {
		return nil, sharedDomain.NewValidationError(promoErrors.ErrPromotionCodeNotFound)
	}
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	p, err := uc.Promotions.GetByCode(tx, in.GymID, codeLower)
	if err != nil {
		return nil, err
	}
	if in.Today != nil {
		// Día LOCAL del gym — con el instante UTC a pelo, una promo vigente
		// su último día se juzgaba vencida desde las 6 PM en caja (CDMX).
		day := localPromoDay(tx, uc.Gyms, in.GymID, *in.Today)
		if err := p.IsCurrentlyValid(day); err != nil {
			return nil, sharedDomain.NewBusinessError(err, "")
		}
		if p.MaxUsesTotal != nil {
			used, err := uc.Applied.CountByPromotion(tx, p.ID)
			if err != nil {
				return nil, sharedDomain.NewUnexpectedError(err)
			}
			if used >= *p.MaxUsesTotal {
				return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionUsageLimitExceeded, "")
			}
		}
	}
	return p, nil
}
