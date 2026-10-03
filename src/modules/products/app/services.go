// Package app holds the products use cases plus a small ProductService that
// other BCs (billing/UC-025, billing/UC-026) plug into. The seam mirrors
// `members.MemberService` so callers stay symmetric.
package app

import (
	"context"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"time"
)

// ProductService is the cross-BC seam for `products`. Other BCs invoke its
// methods inside their own UnitOfWork.Command transactions.
type ProductService struct {
	Products       prodRepo.ProductRepository
	StockMovements prodRepo.StockMovementRepository
}

func NewProductService(products prodRepo.ProductRepository, movements prodRepo.StockMovementRepository) *ProductService {
	return &ProductService{Products: products, StockMovements: movements}
}

// DecrementForSaleInput backs the "products" half of UC-025. Called once per
// line item, inside billing's UoW.Command tx.
type DecrementForSaleInput struct {
	GymID      uuid.UUID
	ProductID  uuid.UUID
	Quantity   int
	OperatorID uuid.UUID
	SaleItemID uuid.UUID
}

type DecrementForSaleOutput struct {
	Product  *productDomain.Product
	Movement *stockMovementDomain.StockMovement
	// UnitCost is the cost known immediately before the sale. nil is an
	// explicit completeness signal; callers must never substitute zero.
	UnitCost *float64
}

type SaleCorrectionSnapshot struct {
	Product  *productDomain.Product
	UnitCost *float64
}

// SnapshotForSaleCorrection resolves the current catalogue snapshot for a new
// or replaced correction line. Existing unchanged lines keep their original
// frozen snapshots and do not call this method.
func (s *ProductService) SnapshotForSaleCorrection(tx sharedDomain.Transaction, gymID, productID uuid.UUID) (*SaleCorrectionSnapshot, error) {
	p, err := s.Products.GetByID(tx, productID)
	if err != nil {
		return nil, err
	}
	if p.GymID != gymID {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
	}
	if !p.Active {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrProductInactive, "")
	}
	cost, err := s.Products.GetUnitCost(tx, gymID, productID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &SaleCorrectionSnapshot{Product: p, UnitCost: cost}, nil
}

type AdjustForSaleCorrectionInput struct {
	GymID, ProductID, OperatorID, SaleItemID uuid.UUID
	// StockDelta = previously-recorded sold qty - corrected sold qty.
	// Positive restores units; negative consumes additional units.
	StockDelta int
	Reason     string
}

func (s *ProductService) AdjustForSaleCorrection(ctx context.Context, tx sharedDomain.Transaction, in AdjustForSaleCorrectionInput, now time.Time) error {
	if in.StockDelta == 0 {
		return nil
	}
	p, err := s.Products.GetByID(tx, in.ProductID)
	if err != nil {
		return err
	}
	if p.GymID != in.GymID {
		return sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
	}
	if in.StockDelta > 0 {
		err = p.IncrementStock(in.StockDelta, now)
	} else {
		err = p.Sell(-in.StockDelta, now)
	}
	if err != nil {
		if err == prodErrors.ErrInsufficientStock {
			return sharedDomain.NewBusinessError(err, "")
		}
		return sharedDomain.NewValidationError(err)
	}
	if _, err := s.Products.Update(tx, p); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	reason := in.Reason
	lineID := in.SaleItemID
	movement, err := stockMovementDomain.New(uuid.New(), in.GymID, p.ID, in.OperatorID,
		stockMovementDomain.TypeCountCorrection, in.StockDelta, &reason, nil, &lineID, now)
	if err != nil {
		return sharedDomain.NewValidationError(err)
	}
	movement.MarkAsCapture() // administrative correction, never a purchase.
	if _, err := s.StockMovements.Create(tx, movement); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	return nil
}

// DecrementForSale records the sale, including negative stock, updates the product row
// AND writes a 'sale' stock_movement so the audit trail is intact. Returns
// the now-current product (for the caller to snapshot price/name).
func (s *ProductService) DecrementForSale(ctx context.Context, tx sharedDomain.Transaction, in DecrementForSaleInput, now time.Time) (*DecrementForSaleOutput, error) {
	if in.Quantity <= 0 {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidAdjustment)
	}
	p, err := s.Products.GetByID(tx, in.ProductID)
	if err != nil {
		return nil, err
	}
	if p.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
	}
	if !p.Active {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrProductInactive, "")
	}
	unitCost, err := s.Products.GetUnitCost(tx, in.GymID, p.ID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if err := p.Sell(in.Quantity, now); err != nil {
		if err == prodErrors.ErrInsufficientStock {
			return nil, sharedDomain.NewBusinessError(err, "")
		}
		return nil, sharedDomain.NewValidationError(err)
	}
	if _, err := s.Products.Update(tx, p); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	saleItemID := in.SaleItemID
	mv, err := stockMovementDomain.New(uuid.New(), in.GymID, p.ID, in.OperatorID,
		stockMovementDomain.TypeSale, -in.Quantity, nil, nil, &saleItemID, now)
	if err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}
	if _, err := s.StockMovements.Create(tx, mv); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &DecrementForSaleOutput{Product: p, Movement: mv, UnitCost: unitCost}, nil
}

// IncrementForRefundInput backs UC-026 (rare in MVP — see Sesión 4 brief).
// Increments stock and writes a 'refund' stock_movement.
type IncrementForRefundInput struct {
	GymID      uuid.UUID
	ProductID  uuid.UUID
	Quantity   int
	OperatorID uuid.UUID
	SaleItemID uuid.UUID
}

func (s *ProductService) IncrementForRefund(ctx context.Context, tx sharedDomain.Transaction, in IncrementForRefundInput, now time.Time) error {
	if in.Quantity <= 0 {
		return sharedDomain.NewValidationError(prodErrors.ErrInvalidAdjustment)
	}
	p, err := s.Products.GetByID(tx, in.ProductID)
	if err != nil {
		return err
	}
	if p.GymID != in.GymID {
		return sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
	}
	if err := p.IncrementStock(in.Quantity, now); err != nil {
		return sharedDomain.NewValidationError(err)
	}
	if _, err := s.Products.Update(tx, p); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	saleItemID := in.SaleItemID
	mv, err := stockMovementDomain.New(uuid.New(), in.GymID, p.ID, in.OperatorID,
		stockMovementDomain.TypeRefund, in.Quantity, nil, nil, &saleItemID, now)
	if err != nil {
		return sharedDomain.NewValidationError(err)
	}
	if _, err := s.StockMovements.Create(tx, mv); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	return nil
}
