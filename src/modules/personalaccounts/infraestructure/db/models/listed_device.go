package models

import (
	"time"

	"github.com/google/uuid"
)

// Read-only projection, never passed to Create/Save/Updates.
type ListedDeviceModel struct {
	ID           uuid.UUID  `gorm:"column:id"`
	AccountID    uuid.UUID  `gorm:"column:account_id"`
	Platform     string     `gorm:"column:platform"`
	Label        string     `gorm:"column:label"`
	AuthorizedAt time.Time  `gorm:"column:authorized_at"`
	RevokedAt    *time.Time `gorm:"column:revoked_at"`
	LastSeenAt   *time.Time `gorm:"column:last_seen_at"`
}

func (ListedDeviceModel) TableName() string { return "mancuerna.devices" }
