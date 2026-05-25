package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	promoErrors "github.com/cuadra/cuadra-core/src/modules/promotions/domain/errors"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type UpdatePromotionInput struct {
	GymID            uuid.UUID
	ActorUserID      uuid.UUID
	PromotionID      uuid.UUID
	Name             string
	Description      *string
	Kind             string
	Value            *float64
	CompanionCount   *int
	AppliesTo        string
	Code             *string
	ValidFrom        *time.Time
	ValidUntil       *time.Time
	MaxUsesTotal     *int
	MaxUsesPerMember *int
}

type UpdatePromotion struct {
	Repo  promoRepo.PromotionRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewUpdatePromotion(repo promoRepo.PromotionRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdatePromotion {
	return &UpdatePromotion{Repo: repo, UoW: uow, Audit: recorder}
}

func (uc *UpdatePromotion) Execute(ctx context.Context, in UpdatePromotionInput) (*promoDomain.Promotion, error) {
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
		// Code unique check (case-insensitive, excluyendo nuestra propia fila).
		if in.Code != nil {
			codeLower := strings.ToLower(strings.TrimSpace(*in.Code))
			if codeLower != "" {
				exists, err := uc.Repo.ExistsByCode(tx, in.GymID, codeLower, &p.ID)
				if err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if exists {
					return sharedDomain.NewBusinessError(promoErrors.ErrPromotionCodeDuplicate, "")
				}
			}
		}
		before := map[string]any{
			"name": p.Name, "kind": p.Kind, "value": p.Value, "active": p.Active,
		}
		if err := p.Update(promoDomain.NewParams{
			Name:             in.Name,
			Description:      in.Description,
			Kind:             in.Kind,
			Value:            in.Value,
			CompanionCount:   in.CompanionCount,
			AppliesTo:        in.AppliesTo,
			Code:             in.Code,
			ValidFrom:        in.ValidFrom,
			ValidUntil:       in.ValidUntil,
			MaxUsesTotal:     in.MaxUsesTotal,
			MaxUsesPerMember: in.MaxUsesPerMember,
		}, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
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
			Changes:     map[string]any{"before": before, "after": map[string]any{"name": updated.Name, "kind": updated.Kind, "value": updated.Value}},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		out = updated
		return nil
	})
	return out, err
}
