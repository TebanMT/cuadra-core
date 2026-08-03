//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// StockMovementModel mirrors `stock_movements` (ADR-002 §3.11). Append-only.
type StockMovementModel struct {
	ID           uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID        uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version      int        `gorm:"not null;default:1;column:version"`
	CreatedAt    time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt    time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt    *time.Time `gorm:"column:deleted_at"`
	ProductID    uuid.UUID  `gorm:"type:uuid;not null;column:product_id"`
	MovementType string     `gorm:"not null;column:movement_type"`
	Delta        int        `gorm:"not null;column:delta"`
	Reason       *string    `gorm:"column:reason"`
	Cost         *float64   `gorm:"type:numeric(12,2);column:cost"`
	IsPurchase   bool       `gorm:"not null;default:true;column:is_purchase"`
	SaleItemID   *uuid.UUID `gorm:"type:uuid;column:sale_item_id"`
	OperatorID   uuid.UUID  `gorm:"type:uuid;not null;column:operator_id"`
}

func (StockMovementModel) TableName() string { return "stock_movements" }
