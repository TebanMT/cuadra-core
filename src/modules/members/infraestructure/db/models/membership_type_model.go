//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

type MembershipTypeModel struct {
	ID                   uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID                uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version              int        `gorm:"not null;default:1;column:version"`
	CreatedAt            time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt            time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt            *time.Time `gorm:"column:deleted_at"`
	Name                 string     `gorm:"not null;column:name"`
	Price                float64    `gorm:"type:numeric(12,2);not null;column:price"`
	DurationDays         int        `gorm:"not null;column:duration_days"`
	EnrollmentFee        float64    `gorm:"type:numeric(12,2);not null;default:0;column:enrollment_fee"`
	MaintenanceFee       float64    `gorm:"type:numeric(12,2);not null;default:0;column:maintenance_fee"`
	MaintenanceFrequency *string    `gorm:"column:maintenance_frequency"`
	Active               bool       `gorm:"not null;default:true;column:active"`
}

func (MembershipTypeModel) TableName() string { return "membership_types" }
