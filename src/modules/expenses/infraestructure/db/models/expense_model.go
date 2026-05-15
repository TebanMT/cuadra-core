//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// ExpenseModel mirrors `expenses` (migration 015). El dominio se mantiene
// libre de tags GORM; el mapper en repositories/ los bridgea.
type ExpenseModel struct {
	ID            uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID         uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version       int        `gorm:"not null;default:1;column:version"`
	CreatedAt     time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt     time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt     *time.Time `gorm:"column:deleted_at"`
	ExpenseDate   time.Time  `gorm:"type:date;not null;column:expense_date"`
	Amount        float64    `gorm:"type:numeric(12,2);not null;column:amount"`
	Category      string     `gorm:"not null;column:category"`
	Description   *string    `gorm:"column:description"`
	PaymentMethod string     `gorm:"not null;column:payment_method"`
	CreatedBy     uuid.UUID  `gorm:"type:uuid;not null;column:created_by"`
}

func (ExpenseModel) TableName() string { return "expenses" }
