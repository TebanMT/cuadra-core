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

// DeactivateProductInput backs UC-023 (DELETE — soft). The product becomes
// invisible to the venta UI grid but historic sale_items keep their FK.
type DeactivateProductInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	ProductID   uuid.UUID
}

type DeactivateProduct struct {
	Products prodRepo.ProductRepository
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
}

func NewDeactivateProduct(products prodRepo.ProductRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *DeactivateProduct {
	return &DeactivateProduct{Products: products, UoW: uow, Audit: recorder}
}

func (uc *DeactivateProduct) Execute(ctx context.Context, in DeactivateProductInput) error {
	now := time.Now().UTC()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		p, err := uc.Products.GetByID(tx, in.ProductID)
		if err != nil {
			return err
		}
		if p.GymID != in.GymID {
			return sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
		}
		if !p.Active {
			return nil
		}
		p.Deactivate(now)
		if _, err := uc.Products.Update(tx, p); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "products",
			EntityID:    p.ID,
			Action:      audit.ActionDelete,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"name": p.Name},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
}
