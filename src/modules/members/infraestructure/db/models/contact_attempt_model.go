//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// ContactAttemptModel mirrors `contact_attempts` (UC-035).
type ContactAttemptModel struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID       uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version     int        `gorm:"not null;default:1;column:version"`
	CreatedAt   time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt   time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt   *time.Time `gorm:"column:deleted_at"`
	MemberID    uuid.UUID  `gorm:"type:uuid;not null;column:member_id"`
	AttemptAt   time.Time  `gorm:"not null;column:attempt_at"`
	Channel     *string    `gorm:"column:channel"`
	Note        *string    `gorm:"column:note"`
	ContactedBy uuid.UUID  `gorm:"type:uuid;not null;column:contacted_by"`
}

func (ContactAttemptModel) TableName() string { return "contact_attempts" }
