package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	promoErrors "github.com/cuadra/cuadra-core/src/modules/promotions/domain/errors"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type DeactivatePromotionInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	PromotionID uuid.UUID
}

type DeactivatePromotion struct {
	Repo  promoRepo.PromotionRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewDeactivatePromotion(repo promoRepo.PromotionRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *DeactivatePromotion {
	return &DeactivatePromotion{Repo: repo, UoW: uow, Audit: recorder}
}

func (uc *DeactivatePromotion) Execute(ctx context.Context, in DeactivatePromotionInput) (*promoDomain.Promotion, error) {
	now := time.Now().UTC()
	var out *promoDomain.Promotion
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		p, err := uc.Repo.GetByID(tx, in.PromotionID)
		if err != nil {
			return err
		}
		if p.GymID != in.GymID {
			return sharedDomain.NewBusinessError(promoErrors.ErrCrossGym, "")
		}
		p.Deactivate(now)
		updated, err := uc.Repo.Update(tx, p)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "promotions",
			EntityID:    updated.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"action": "deactivate"},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = updated
		return nil
	})
	return out, err
}

type ReactivatePromotion struct {
	Repo  promoRepo.PromotionRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewReactivatePromotion(repo promoRepo.PromotionRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *ReactivatePromotion {
	return &ReactivatePromotion{Repo: repo, UoW: uow, Audit: recorder}
}

func (uc *ReactivatePromotion) Execute(ctx context.Context, in DeactivatePromotionInput) (*promoDomain.Promotion, error) {
	now := time.Now().UTC()
	var out *promoDomain.Promotion
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		p, err := uc.Repo.GetByID(tx, in.PromotionID)
		if err != nil {
			return err
		}
		if p.GymID != in.GymID {
			return sharedDomain.NewBusinessError(promoErrors.ErrCrossGym, "")
		}
		p.Reactivate(now)
		updated, err := uc.Repo.Update(tx, p)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "promotions",
			EntityID:    updated.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"action": "reactivate"},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = updated
		return nil
	})
	return out, err
}
