//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"errors"
	product "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func (r *ProductSQLiteRepository) LockCount(tx shared.Transaction, gym, id uuid.UUID) error {
	// Acquire the SQLite writer before the idempotency/snapshot reads.
	_, err := tx.(*shared.SqlxTransaction).Exec(context.Background(), `UPDATE products SET id=id WHERE gym_id=? AND id=?`, gym.String(), id.String())
	return err
}
func (r *ProductSQLiteRepository) CountProduct(tx shared.Transaction, gym, id uuid.UUID) (*product.Product, error) {
	var m sqliteProductRow
	err := tx.(*shared.SqlxTransaction).Get(context.Background(), &m, `SELECT * FROM products WHERE gym_id=? AND id=?`, gym.String(), id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return productFromRow(&m), nil
}
