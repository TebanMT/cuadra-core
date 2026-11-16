package models

import (
	"github.com/google/uuid"
	"time"
)

// Only security-transition columns; the profile aggregate cannot write them.
type DeletionAccountModel struct {
	ID              uuid.UUID  `gorm:"column:id"`
	Status          string     `gorm:"column:status"`
	SecurityVersion int64      `gorm:"column:security_version"`
	RequestedAt     *time.Time `gorm:"column:deletion_requested_at"`
	PolicyVersion   *string    `gorm:"column:deletion_policy_version"`
	PurgeAfter      *time.Time `gorm:"column:purge_after"`
	DeletedAt       *time.Time `gorm:"column:deleted_at"`
}

func (DeletionAccountModel) TableName() string { return "mancuerna.accounts" }
