//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// SaleModel mirrors `sales` (ADR-002 §3.10).
type SaleModel struct {
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID     uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version   int        `gorm:"not null;default:1;column:version"`
	CreatedAt time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt *time.Time `gorm:"column:deleted_at"`
	PaymentID uuid.UUID  `gorm:"type:uuid;not null;column:payment_id"`
	MemberID  *uuid.UUID `gorm:"type:uuid;column:member_id"`
	Subtotal  float64    `gorm:"type:numeric(12,2);not null;column:subtotal"`
	Discount  float64    `gorm:"type:numeric(12,2);not null;default:0;column:discount"`
	Total     float64    `gorm:"type:numeric(12,2);not null;column:total"`
}

func (SaleModel) TableName() string { return "sales" }

// SaleItemModel mirrors `sale_items` (ADR-002 §3.10).
type SaleItemModel struct {
	ID                  uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID               uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version             int        `gorm:"not null;default:1;column:version"`
	CreatedAt           time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt           time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt           *time.Time `gorm:"column:deleted_at"`
	SaleID              uuid.UUID  `gorm:"type:uuid;not null;column:sale_id"`
	ProductID           uuid.UUID  `gorm:"type:uuid;not null;column:product_id"`
	ProductNameSnapshot string     `gorm:"not null;column:product_name_snapshot"`
	UnitPriceSnapshot   float64    `gorm:"type:numeric(12,2);not null;column:unit_price_snapshot"`
	Quantity            int        `gorm:"not null;column:quantity"`
	LineTotal           float64    `gorm:"type:numeric(12,2);not null;column:line_total"`
}

func (SaleItemModel) TableName() string { return "sale_items" }
