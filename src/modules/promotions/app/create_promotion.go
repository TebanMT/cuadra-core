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

// CreatePromotionInput refleja el form. Code se valida unique-per-gym
// case-insensitive en el use case (no en el dominio porque requiere
// query al repo).
type CreatePromotionInput struct {
	GymID            uuid.UUID
	ActorUserID      uuid.UUID
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

type CreatePromotion struct {
	Repo  promoRepo.PromotionRepository
	UoW   sharedDomain.UnitOfWork
	Audit audit.Recorder
}

func NewCreatePromotion(repo promoRepo.PromotionRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreatePromotion {
	return &CreatePromotion{Repo: repo, UoW: uow, Audit: recorder}
}

func (uc *CreatePromotion) Execute(ctx context.Context, in CreatePromotionInput) (*promoDomain.Promotion, error) {
	now := time.Now().UTC()
	id := uuid.New()
	p, err := promoDomain.New(id, in.GymID, promoDomain.NewParams{
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
	}, now)
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}

	var out *promoDomain.Promotion
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		if p.Code != nil {
			codeLower := strings.ToLower(strings.TrimSpace(*p.Code))
			exists, err := uc.Repo.ExistsByCode(tx, in.GymID, codeLower, nil)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if exists {
				return sharedDomain.NewBusinessError(promoErrors.ErrPromotionCodeDuplicate, "")
			}
		}
		created, err := uc.Repo.Create(tx, p)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "promotions",
			EntityID:    created.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"name":       created.Name,
				"kind":       created.Kind,
				"applies_to": created.AppliesTo,
				"value":      created.Value,
				"code":       created.Code,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = created
		return nil
	})
	return out, err
}
