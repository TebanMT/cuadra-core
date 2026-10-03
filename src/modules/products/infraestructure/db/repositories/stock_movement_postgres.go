//go:build server

package repositories

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	"github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type StockMovementPostgresRepository struct{}

func NewStockMovementPostgresRepository() *StockMovementPostgresRepository {
	return &StockMovementPostgresRepository{}
}

func (r *StockMovementPostgresRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*stockMovementDomain.StockMovement, error) {
	var row models.StockMovementModel
	err := tx.(*sharedDomain.GormTransaction).Tx.
		Where("gym_id=? AND idempotency_key=? AND deleted_at IS NULL", gymID, strings.TrimSpace(key)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return stockMovementFromModel(&row), nil
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
	row := models.StockMovementModel{
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
		IsPurchase:   m.IsPurchase,
		SaleItemID:   m.SaleItemID,
		OperatorID:   m.OperatorID,
	}
	if m.IdempotencyKey != "" {
		key := m.IdempotencyKey
		row.IdempotencyKey = &key
	}
	if m.IdempotencyFingerprint != "" {
		fingerprint := m.IdempotencyFingerprint
		row.IdempotencyFingerprint = &fingerprint
	}
	if len(m.IdempotencyResult) > 0 {
		result := string(m.IdempotencyResult)
		row.IdempotencyResult = &result
	}
	return row
}

func stockMovementFromModel(m *models.StockMovementModel) *stockMovementDomain.StockMovement {
	out := &stockMovementDomain.StockMovement{
		ID:           m.ID,
		GymID:        m.GymID,
		Version:      m.Version,
		ProductID:    m.ProductID,
		MovementType: m.MovementType,
		Delta:        m.Delta,
		Reason:       m.Reason,
		Cost:         m.Cost,
		IsPurchase:   m.IsPurchase,
		SaleItemID:   m.SaleItemID,
		OperatorID:   m.OperatorID,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
		DeletedAt:    m.DeletedAt,
	}
	if m.IdempotencyKey != nil {
		out.IdempotencyKey = *m.IdempotencyKey
	}
	if m.IdempotencyFingerprint != nil {
		out.IdempotencyFingerprint = *m.IdempotencyFingerprint
	}
	if m.IdempotencyResult != nil {
		out.IdempotencyResult = append([]byte(nil), (*m.IdempotencyResult)...)
	}
	return out
}
