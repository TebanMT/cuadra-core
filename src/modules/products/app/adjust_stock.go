package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// AdjustStockInput backs UC-024. Three movement types:
//
//	'restock'          -> stock += quantity, optional cost
//	'shrinkage'        -> stock -= quantity (rejected if would underflow)
//	'count_correction' -> stock = quantity (replaces, NOT adds)
//
// `Reason` is optional but encouraged.
type AdjustStockInput struct {
	GymID        uuid.UUID
	ActorUserID  uuid.UUID
	ProductID    uuid.UUID
	MovementType string
	Quantity     int      // restock/shrinkage = delta size; count_correction = absolute
	Cost         *float64 // optional, only meaningful for restock
	// IsPurchase — sólo significativo para restock: false = inventario que
	// ya existía físicamente (no salió dinero, no es egreso del período).
	// nil/true = compra (default).
	IsPurchase *bool
	Reason     *string
}

type AdjustStockOutput struct {
	NewStock   int
	Delta      int
	MovementID uuid.UUID
}

type AdjustStock struct {
	Products       prodRepo.ProductRepository
	StockMovements prodRepo.StockMovementRepository
	UoW            sharedDomain.UnitOfWork
	Audit          audit.Recorder
}

func NewAdjustStock(products prodRepo.ProductRepository, movements prodRepo.StockMovementRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *AdjustStock {
	return &AdjustStock{Products: products, StockMovements: movements, UoW: uow, Audit: recorder}
}

func (uc *AdjustStock) Execute(ctx context.Context, in AdjustStockInput) (*AdjustStockOutput, error) {
	if !stockMovementDomain.ValidType(in.MovementType) ||
		in.MovementType == stockMovementDomain.TypeSale ||
		in.MovementType == stockMovementDomain.TypeRefund {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidMovementType)
	}
	now := time.Now().UTC()
	var out AdjustStockOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		p, err := uc.Products.GetByID(tx, in.ProductID)
		if err != nil {
			return err
		}
		if p.GymID != in.GymID {
			return sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
		}
		var delta int
		switch in.MovementType {
		case stockMovementDomain.TypeRestock:
			if err := p.IncrementStock(in.Quantity, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			delta = in.Quantity
		case stockMovementDomain.TypeShrinkage:
			if err := p.DecrementStock(in.Quantity, now); err != nil {
				if err == prodErrors.ErrInsufficientStock {
					return sharedDomain.NewBusinessError(err, "")
				}
				return sharedDomain.NewValidationError(err)
			}
			delta = -in.Quantity
		case stockMovementDomain.TypeCountCorrection:
			d, err := p.SetStock(in.Quantity, now)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			delta = d
		}
		if _, err := uc.Products.Update(tx, p); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		var cost *float64
		if in.MovementType == stockMovementDomain.TypeRestock {
			cost = in.Cost
		}
		mv, err := stockMovementDomain.New(uuid.New(), in.GymID, p.ID, in.ActorUserID,
			in.MovementType, delta, in.Reason, cost, nil, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if in.MovementType == stockMovementDomain.TypeRestock &&
			in.IsPurchase != nil && !*in.IsPurchase {
			mv.MarkAsCapture()
		}
		if _, err := uc.StockMovements.Create(tx, mv); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "stock_movements",
			EntityID:    mv.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"product_id":    p.ID,
				"movement_type": mv.MovementType,
				"delta":         mv.Delta,
				"new_stock":     p.Stock,
				"reason":        mv.Reason,
				"cost":          mv.Cost,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = AdjustStockOutput{NewStock: p.Stock, Delta: delta, MovementID: mv.ID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
