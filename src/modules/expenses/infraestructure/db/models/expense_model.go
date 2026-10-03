//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// ExpenseModel mirrors `expenses` (migration 015). El dominio se mantiene
// libre de tags GORM; el mapper en repositories/ los bridgea.
type ExpenseModel struct {
	ID                    uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID                 uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version               int        `gorm:"not null;default:1;column:version"`
	CreatedAt             time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt             time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt             *time.Time `gorm:"column:deleted_at"`
	ExpenseDate           time.Time  `gorm:"type:date;not null;column:expense_date"`
	Amount                float64    `gorm:"type:numeric(12,2);not null;column:amount"`
	Category              string     `gorm:"not null;column:category"`
	PayeeName             *string    `gorm:"column:payee_name"`
	Description           *string    `gorm:"column:description"`
	Reference             *string    `gorm:"column:reference"`
	PaymentMethod         string     `gorm:"not null;column:payment_method"`
	PaidFrom              string     `gorm:"not null;column:paid_from"`
	Classification        string     `gorm:"not null;column:classification"`
	Source                string     `gorm:"not null;column:source"`
	RecurringOccurrenceID *uuid.UUID `gorm:"type:uuid;column:recurring_occurrence_id"`
	CashMovementID        *uuid.UUID `gorm:"type:uuid;column:cash_movement_id"`
	CreatedBy             uuid.UUID  `gorm:"type:uuid;not null;column:created_by"`
}

func (ExpenseModel) TableName() string { return "expenses" }
