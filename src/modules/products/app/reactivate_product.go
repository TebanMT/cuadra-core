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

// ReactivateProductInput is the inverse of UC-023's deactivate path. The
// operator usually re-enables a product they took off the venta UI grid (e.g.
// the product is back in stock or was deactivated by mistake).
type ReactivateProductInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	ProductID   uuid.UUID
}

type ReactivateProduct struct {
	Products prodRepo.ProductRepository
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
}

func NewReactivateProduct(products prodRepo.ProductRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *ReactivateProduct {
	return &ReactivateProduct{Products: products, UoW: uow, Audit: recorder}
}

func (uc *ReactivateProduct) Execute(ctx context.Context, in ReactivateProductInput) error {
	now := time.Now().UTC()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		p, err := uc.Products.GetByID(tx, in.ProductID)
		if err != nil {
			return err
		}
		if p.GymID != in.GymID {
			return sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
		}
		if p.Active {
			return nil
		}
		p.Reactivate(now)
		if _, err := uc.Products.Update(tx, p); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "products",
			EntityID:    p.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"active": true, "name": p.Name},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
}
