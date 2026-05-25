//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// MembershipModel mirrors `memberships` (ADR-002 §3.6). Snapshot fields are
// frozen at insert — DA-11.1.
type MembershipModel struct {
	ID                     uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID                  uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version                int        `gorm:"not null;default:1;column:version"`
	CreatedAt              time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt              time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt              *time.Time `gorm:"column:deleted_at"`
	MemberID               uuid.UUID  `gorm:"type:uuid;not null;column:member_id"`
	MembershipTypeID       uuid.UUID  `gorm:"type:uuid;not null;column:membership_type_id"`
	TypeNameSnapshot       string     `gorm:"not null;column:type_name_snapshot"`
	PriceSnapshot          float64    `gorm:"type:numeric(12,2);not null;column:price_snapshot"`
	DurationDaysSnapshot   int        `gorm:"not null;column:duration_days_snapshot"`
	DurationMonthsSnapshot *int       `gorm:"column:duration_months_snapshot"`
	StartDate              time.Time  `gorm:"type:date;not null;column:start_date"`
	// ExpiryDate es nullable: nil cuando status = pending_payment.
	ExpiryDate *time.Time `gorm:"type:date;column:expiry_date"`
	Status     string     `gorm:"not null;default:active;column:status"`
	ReplacedBy *uuid.UUID `gorm:"type:uuid;column:replaced_by"`
}

func (MembershipModel) TableName() string { return "memberships" }

// MembershipAdjustmentModel mirrors `membership_adjustments` (ADR-002 §3.7).
type MembershipAdjustmentModel struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID          uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version        int        `gorm:"not null;default:1;column:version"`
	CreatedAt      time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt      time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt      *time.Time `gorm:"column:deleted_at"`
	MembershipID   uuid.UUID  `gorm:"type:uuid;not null;column:membership_id"`
	AdjustedBy     uuid.UUID  `gorm:"type:uuid;not null;column:adjusted_by"`
	Reason         string     `gorm:"not null;column:reason"`
	DaysAdded      int        `gorm:"not null;column:days_added"`
	PreviousExpiry time.Time  `gorm:"type:date;not null;column:previous_expiry"`
	NewExpiry      time.Time  `gorm:"type:date;not null;column:new_expiry"`
}

func (MembershipAdjustmentModel) TableName() string { return "membership_adjustments" }
