//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

type InventoryPurchaseModel struct {
	Origin          string     `gorm:"column:origin;not null;default:desktop"`
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID           uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version         int        `gorm:"not null;default:1;column:version"`
	CreatedAt       time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt       time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt       *time.Time `gorm:"column:deleted_at"`
	StockMovementID *uuid.UUID `gorm:"type:uuid;column:stock_movement_id"`
	ProductID       uuid.UUID  `gorm:"type:uuid;not null;column:product_id"`
	Quantity        int        `gorm:"not null;column:quantity"`
	UnitCost        *float64   `gorm:"type:numeric(12,2);column:unit_cost"`
	TotalAmount     *float64   `gorm:"type:numeric(12,2);column:total_amount"`
	Status          string     `gorm:"not null;column:status"`
	PaidOn          *time.Time `gorm:"type:date;column:paid_on"`
	PaymentMethod   *string    `gorm:"column:payment_method"`
	PaidFrom        *string    `gorm:"column:paid_from"`
	CashMovementID  *uuid.UUID `gorm:"type:uuid;column:cash_movement_id"`
	IdempotencyKey  string     `gorm:"not null;column:idempotency_key"`
	CreatedBy       uuid.UUID  `gorm:"type:uuid;not null;column:created_by"`
}

func (InventoryPurchaseModel) TableName() string { return "inventory_purchases" }
