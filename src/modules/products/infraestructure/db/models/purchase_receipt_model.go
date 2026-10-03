//go:build server

package models

import (
	"github.com/google/uuid"
	"time"
)

type PurchaseReceiptModel struct {
	ID         uuid.UUID `gorm:"column:id;primaryKey"`
	GymID      uuid.UUID `gorm:"column:gym_id"`
	PurchaseID uuid.UUID `gorm:"column:purchase_id"`
	ProductID  uuid.UUID `gorm:"column:product_id"`
	ReceivedBy uuid.UUID `gorm:"column:received_by"`
	Quantity   int       `gorm:"column:quantity"`
	UnitCost   float64   `gorm:"column:unit_cost"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (PurchaseReceiptModel) TableName() string { return "inventory_purchase_receipts" }
