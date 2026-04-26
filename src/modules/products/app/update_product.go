package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// UpdateProductInput backs UC-023 (edición). Stock changes go through the
// dedicated UC-024 path, NOT through here.
type UpdateProductInput struct {
	GymID        uuid.UUID
	ActorUserID  uuid.UUID
	ProductID    uuid.UUID
	Name         string
	Price        float64
	StockMinimum int
	Category     *string
	ImageURL     *string
}

type UpdateProduct struct {
	Products prodRepo.ProductRepository
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
}

func NewUpdateProduct(products prodRepo.ProductRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateProduct {
	return &UpdateProduct{Products: products, UoW: uow, Audit: recorder}
}

func (uc *UpdateProduct) Execute(ctx context.Context, in UpdateProductInput) error {
	now := time.Now().UTC()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		p, err := uc.Products.GetByID(tx, in.ProductID)
		if err != nil {
			return err
		}
		if p.GymID != in.GymID {
			return sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
		}
		exists, err := uc.Products.ExistsByGymAndName(tx, in.GymID, in.Name, &in.ProductID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if exists {
			return sharedDomain.NewBusinessError(prodErrors.ErrNameAlreadyExists, "")
		}
		before := map[string]any{
			"name":          p.Name,
			"price":         p.Price,
			"stock_minimum": p.StockMinimum,
			"category":      p.Category,
			"image_url":     p.ImageURL,
		}
		if err := p.Update(in.Name, in.Price, in.StockMinimum, in.Category, in.ImageURL, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err := uc.Products.Update(tx, p); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "products",
			EntityID:    p.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"before": before,
				"after": map[string]any{
					"name":          p.Name,
					"price":         p.Price,
					"stock_minimum": p.StockMinimum,
					"category":      p.Category,
					"image_url":     p.ImageURL,
				},
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		return nil
	})
}
