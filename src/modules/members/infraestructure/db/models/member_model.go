//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// MemberModel mirrors `members` (ADR-002 §3.5). Domain entity — Member —
// stays free of GORM tags; the mapper in repositories/ bridges them.
type MemberModel struct {
	ID                   uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID                uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version              int        `gorm:"not null;default:1;column:version"`
	CreatedAt            time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt            time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt            *time.Time `gorm:"column:deleted_at"`
	Folio                string     `gorm:"not null;column:folio"`
	FullName             string     `gorm:"not null;column:full_name"`
	Phone                string     `gorm:"not null;column:phone"`
	Email                *string    `gorm:"column:email"`
	Birthdate            *time.Time `gorm:"type:date;column:birthdate"`
	PhotoURL             *string    `gorm:"column:photo_url"`
	Notes                *string    `gorm:"column:notes"`
	Status               string     `gorm:"not null;default:active;column:status"`
	EnrollmentPaid       bool       `gorm:"not null;default:false;column:enrollment_paid"`
	LastMaintenancePaid  *time.Time `gorm:"type:date;column:last_maintenance_paid"`
	PinHash              *string    `gorm:"column:pin_hash"`
	PinPlain             *string    `gorm:"column:pin_plain"`
	PinAssignedAt        *time.Time `gorm:"column:pin_assigned_at"`
	LastContactAttemptAt *time.Time `gorm:"column:last_contact_attempt_at"`
	CreatedBy            uuid.UUID  `gorm:"type:uuid;not null;column:created_by"`
}

func (MemberModel) TableName() string { return "members" }
