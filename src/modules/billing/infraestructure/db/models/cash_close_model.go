//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// CashCloseEventModel mirrors `cash_close_events` (ADR-002 §3.14).
// `Discrepancy` is a generated column — read-only on this side.
type CashCloseEventModel struct {
	ID                uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID             uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version           int        `gorm:"not null;default:1;column:version"`
	CreatedAt         time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt         time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt         *time.Time `gorm:"column:deleted_at"`
	CloseDate         time.Time  `gorm:"type:date;not null;column:close_date"`
	CalculatedCash    float64    `gorm:"type:numeric(12,2);not null;column:calculated_cash"`
	CountedCash       *float64   `gorm:"type:numeric(12,2);column:counted_cash"`
	DiscrepancyReason *string    `gorm:"column:discrepancy_reason"`
	ClosedBy          uuid.UUID  `gorm:"type:uuid;not null;column:closed_by"`
}

func (CashCloseEventModel) TableName() string { return "cash_close_events" }
