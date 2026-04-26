//go:build server

package repositories

import (
	"github.com/google/uuid"

	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	"github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type StockMovementPostgresRepository struct{}

func NewStockMovementPostgresRepository() *StockMovementPostgresRepository {
	return &StockMovementPostgresRepository{}
}

func (r *StockMovementPostgresRepository) Create(tx sharedDomain.Transaction, m *stockMovementDomain.StockMovement) (*stockMovementDomain.StockMovement, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := stockMovementToModel(m)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return stockMovementFromModel(&row), nil
}

func (r *StockMovementPostgresRepository) ListByProduct(tx sharedDomain.Transaction, productID uuid.UUID, limit int) ([]*stockMovementDomain.StockMovement, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows []models.StockMovementModel
	if err := gormTx.Where("product_id = ? AND deleted_at IS NULL", productID).
		Order("created_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*stockMovementDomain.StockMovement, len(rows))
	for i := range rows {
		out[i] = stockMovementFromModel(&rows[i])
	}
	return out, nil
}

func stockMovementToModel(m *stockMovementDomain.StockMovement) models.StockMovementModel {
	return models.StockMovementModel{
		ID:           m.ID,
		GymID:        m.GymID,
		Version:      m.Version,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
		DeletedAt:    m.DeletedAt,
		ProductID:    m.ProductID,
		MovementType: m.MovementType,
		Delta:        m.Delta,
		Reason:       m.Reason,
		Cost:         m.Cost,
		SaleItemID:   m.SaleItemID,
		OperatorID:   m.OperatorID,
	}
}

func stockMovementFromModel(m *models.StockMovementModel) *stockMovementDomain.StockMovement {
	return &stockMovementDomain.StockMovement{
		ID:           m.ID,
		GymID:        m.GymID,
		Version:      m.Version,
		ProductID:    m.ProductID,
		MovementType: m.MovementType,
		Delta:        m.Delta,
		Reason:       m.Reason,
		Cost:         m.Cost,
		SaleItemID:   m.SaleItemID,
		OperatorID:   m.OperatorID,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
		DeletedAt:    m.DeletedAt,
	}
}
