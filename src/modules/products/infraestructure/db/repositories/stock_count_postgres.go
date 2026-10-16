//go:build server

package repositories

import (
	"errors"
	product "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	"github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *ProductPostgresRepository) LockCount(tx shared.Transaction, gym, id uuid.UUID) error {
	return tx.(*shared.GormTransaction).Tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, gym.String()+":stock-count:"+id.String()).Error
}
func (r *ProductPostgresRepository) CountProduct(tx shared.Transaction, gym, id uuid.UUID) (*product.Product, error) {
	var m models.ProductModel
	err := tx.(*shared.GormTransaction).Tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("gym_id=? AND id=?", gym, id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return productFromModel(&m), nil
}
