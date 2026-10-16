package repository

import (
	product "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// Locks precede reads. PostgreSQL serializes against sales and sync via the
// product row; SQLite acquires its writer lock before reading the snapshot.
type StockCountRepository interface {
	LockCount(shared.Transaction, uuid.UUID, uuid.UUID) error
	CountProduct(shared.Transaction, uuid.UUID, uuid.UUID) (*product.Product, error)
}
