// Package stockmovement holds the StockMovement entity — the event log that
// records every change to product stock. ADR-002 §3.11: the products.stock
// column is denormalised; this table is the source of truth for "how did we
// get to this number?". The aggregate is small and append-only.
package stockmovement

import (
	"strings"
	"time"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
)

// MovementType values mirror chk_stock_movements_type.
const (
	TypeSale            = "sale"
	TypeRestock         = "restock"
	TypeShrinkage       = "shrinkage"
	TypeCountCorrection = "count_correction"
	TypeRefund          = "refund"
)

// StockMovement is one row in the event log. `Delta` is signed: + for stock
// going in (restock, refund), - for stock going out (sale, shrinkage).
// `count_correction` carries the *resulting delta* (new - old) so summing the
// log still reconstructs the current stock.
type StockMovement struct {
	ID           uuid.UUID
	GymID        uuid.UUID
	Version      int
	ProductID    uuid.UUID
	MovementType string
	Delta        int
	Reason       *string
	Cost         *float64
	SaleItemID   *uuid.UUID
	OperatorID   uuid.UUID
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
}

// New constructs a StockMovement. The caller (use case) is responsible for
// having already mutated Product.Stock inside the same transaction.
func New(id, gymID, productID, operatorID uuid.UUID,
	movementType string, delta int,
	reason *string, cost *float64, saleItemID *uuid.UUID,
	now time.Time) (*StockMovement, error) {
	if !ValidType(movementType) {
		return nil, prodErrors.ErrInvalidMovementType
	}
	if reason != nil {
		r := strings.TrimSpace(*reason)
		if r == "" {
			reason = nil
		} else {
			reason = &r
		}
	}
	return &StockMovement{
		ID:           id,
		GymID:        gymID,
		Version:      1,
		ProductID:    productID,
		MovementType: movementType,
		Delta:        delta,
		Reason:       reason,
		Cost:         cost,
		SaleItemID:   saleItemID,
		OperatorID:   operatorID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

// ValidType reports whether the given string matches one of the catalogue.
func ValidType(t string) bool {
	switch t {
	case TypeSale, TypeRestock, TypeShrinkage, TypeCountCorrection, TypeRefund:
		return true
	}
	return false
}
