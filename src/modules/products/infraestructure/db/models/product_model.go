//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// ProductModel mirrors `products` (ADR-002 §3.11). The domain entity stays
// free of GORM tags; the mapper in repositories/ bridges them.
type ProductModel struct {
	ID           uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID        uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version      int        `gorm:"not null;default:1;column:version"`
	CreatedAt    time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt    time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt    *time.Time `gorm:"column:deleted_at"`
	Name         string     `gorm:"not null;column:name"`
	Price        float64    `gorm:"type:numeric(12,2);not null;column:price"`
	Stock        int        `gorm:"not null;default:0;column:stock"`
	StockMinimum int        `gorm:"not null;default:0;column:stock_minimum"`
	Category     *string    `gorm:"column:category"`
	ImageURL     *string    `gorm:"column:image_url"`
	Active       bool       `gorm:"not null;default:true;column:active"`
}

func (ProductModel) TableName() string { return "products" }
