//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

type AuthorizedDeviceModel struct {
	ID                 uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	AccountID          uuid.UUID  `gorm:"column:account_id;type:uuid"`
	Platform           string     `gorm:"column:platform"`
	Label              string     `gorm:"column:label"`
	PublicKey          []byte     `gorm:"column:public_key"`
	AuthorizedAt       time.Time  `gorm:"column:authorized_at"`
	AuthorizedByMethod string     `gorm:"column:authorized_by_method"`
	RevokedAt          *time.Time `gorm:"column:revoked_at"`
	RevocationReason   *string    `gorm:"column:revocation_reason"`
	LastSeenAt         *time.Time `gorm:"column:last_seen_at"`
	CreatedAt          time.Time  `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt          time.Time  `gorm:"column:updated_at;autoUpdateTime:false"`
	DeletedAt          *time.Time `gorm:"column:deleted_at"`
}

func (AuthorizedDeviceModel) TableName() string { return "mancuerna.devices" }

type PersonalSessionModel struct {
	ID                   uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	AccountID            uuid.UUID  `gorm:"column:account_id;type:uuid"`
	DeviceID             uuid.UUID  `gorm:"column:device_id;type:uuid"`
	RefreshHash          []byte     `gorm:"column:refresh_hash"`
	RefreshFamilyID      uuid.UUID  `gorm:"column:refresh_family_id;type:uuid"`
	Generation           int64      `gorm:"column:generation"`
	ExpiresAt            time.Time  `gorm:"column:expires_at"`
	RevocationTicketHash []byte     `gorm:"column:revocation_ticket_hash"`
	RevokedAt            *time.Time `gorm:"column:revoked_at"`
	LastUsedAt           *time.Time `gorm:"column:last_used_at"`
	CreatedAt            time.Time  `gorm:"column:created_at;autoCreateTime:false"`
}

func (PersonalSessionModel) TableName() string { return "mancuerna.sessions" }
