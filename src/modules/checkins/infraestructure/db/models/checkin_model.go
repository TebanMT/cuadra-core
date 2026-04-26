//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// CheckinModel mirrors `checkins` (ADR-002 §3.12). Domain entity stays GORM-free.
type CheckinModel struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID          uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version        int        `gorm:"not null;default:1;column:version"`
	CreatedAt      time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt      time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt      *time.Time `gorm:"column:deleted_at"`
	MemberID       uuid.UUID  `gorm:"type:uuid;not null;column:member_id"`
	CheckinAt      time.Time  `gorm:"not null;column:checkin_at"`
	Method         string     `gorm:"not null;column:method"`
	Result         string     `gorm:"not null;column:result"`
	OperatorID     *uuid.UUID `gorm:"type:uuid;column:operator_id"`
	ManualOverride bool       `gorm:"not null;default:false;column:manual_override"`
	OverrideReason *string    `gorm:"column:override_reason"`
}

func (CheckinModel) TableName() string { return "checkins" }
