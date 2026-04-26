// Package repository declares the persistence contracts for the products BC.
// Concrete impls live in infraestructure/db/repositories with build tags.
package repository

import (
	"github.com/google/uuid"

	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ProductRepository — UC-023, UC-024, and the products half of UC-025.
type ProductRepository interface {
	Create(tx sharedDomain.Transaction, p *productDomain.Product) (*productDomain.Product, error)
	Update(tx sharedDomain.Transaction, p *productDomain.Product) (*productDomain.Product, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*productDomain.Product, error)
	List(tx sharedDomain.Transaction, q ListQuery) ([]*productDomain.Product, int, error)
	ExistsByGymAndName(tx sharedDomain.Transaction, gymID uuid.UUID, name string, excludeID *uuid.UUID) (bool, error)
}

// ListQuery is the input for UC-023 (list products). Filters are intentionally
// minimal — the recepcionist UI is the main caller, the dashboard is secondary.
type ListQuery struct {
	GymID           uuid.UUID
	Search          string // case-insensitive substring on name
	Category        string // exact match; empty = all
	IncludeInactive bool
	LowStockOnly    bool
	Page            int
	PageSize        int
}

// StockMovementRepository — append-only history. Used by UC-024 and UC-025.
type StockMovementRepository interface {
	Create(tx sharedDomain.Transaction, m *stockMovementDomain.StockMovement) (*stockMovementDomain.StockMovement, error)
	ListByProduct(tx sharedDomain.Transaction, productID uuid.UUID, limit int) ([]*stockMovementDomain.StockMovement, error)
}
