package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CreateProductInput backs UC-023 (alta). InitialStock seeds both the
// denormalised counter on `products` and a 'restock' row in `stock_movements`
// (DA-24.1 — the event log is the source of truth even on day zero).
type CreateProductInput struct {
	GymID         uuid.UUID
	ActorUserID   uuid.UUID
	Name          string
	Price         float64
	InitialStock  int
	StockMinimum  int
	Category      *string
	ImageURL      *string
	InitialCost   *float64 // optional — DA-24.3
	InitialReason *string  // free-text, e.g. "Stock inicial"
	// InitialIsPurchase — false = el stock inicial es inventario que YA
	// existía (captura de catálogo, no salió dinero): conserva el costo
	// para el margen pero NO cuenta como egreso del período. nil/true =
	// compra (semántica previa, callers viejos sin el campo).
	InitialIsPurchase *bool
}

type CreateProductOutput struct {
	ProductID uuid.UUID
	Name      string
	Stock     int
}

type CreateProduct struct {
	Products       prodRepo.ProductRepository
	StockMovements prodRepo.StockMovementRepository
	UoW            sharedDomain.UnitOfWork
	Audit          audit.Recorder
}

func NewCreateProduct(products prodRepo.ProductRepository, movements prodRepo.StockMovementRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreateProduct {
	return &CreateProduct{Products: products, StockMovements: movements, UoW: uow, Audit: recorder}
}

func (uc *CreateProduct) Execute(ctx context.Context, in CreateProductInput) (*CreateProductOutput, error) {
	now := time.Now().UTC()
	var out CreateProductOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		exists, err := uc.Products.ExistsByGymAndName(tx, in.GymID, in.Name, nil)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if exists {
			return sharedDomain.NewBusinessError(prodErrors.ErrNameAlreadyExists, "")
		}
		p, err := productDomain.New(uuid.New(), in.GymID, in.Name, in.Price,
			in.InitialStock, in.StockMinimum, in.Category, in.ImageURL, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err := uc.Products.Create(tx, p); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		// Seed event-log only when there's actually stock to log.
		if in.InitialStock > 0 {
			reason := in.InitialReason
			if reason == nil {
				r := "Stock inicial"
				reason = &r
			}
			mv, err := stockMovementDomain.New(uuid.New(), in.GymID, p.ID, in.ActorUserID,
				stockMovementDomain.TypeRestock, in.InitialStock, reason, in.InitialCost, nil, now)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if in.InitialIsPurchase != nil && !*in.InitialIsPurchase {
				mv.MarkAsCapture()
			}
			if _, err := uc.StockMovements.Create(tx, mv); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "products",
			EntityID:    p.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"name":          p.Name,
				"price":         p.Price,
				"stock":         p.Stock,
				"stock_minimum": p.StockMinimum,
				"category":      p.Category,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = CreateProductOutput{ProductID: p.ID, Name: p.Name, Stock: p.Stock}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
