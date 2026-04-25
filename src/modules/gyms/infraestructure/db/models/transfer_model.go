//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

type GymOwnershipTransferModel struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID      uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version    int        `gorm:"not null;default:1;column:version"`
	CreatedAt  time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt  time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt  *time.Time `gorm:"column:deleted_at"`
	FromUserID uuid.UUID  `gorm:"type:uuid;not null;column:from_user_id"`
	ToUserID   uuid.UUID  `gorm:"type:uuid;not null;column:to_user_id"`
	ExecutedAt time.Time  `gorm:"not null;column:executed_at"`
}

func (GymOwnershipTransferModel) TableName() string { return "gym_ownership_transfers" }

type OwnershipTransferOTPModel struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID      uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	FromUserID uuid.UUID  `gorm:"type:uuid;not null;column:from_user_id"`
	ToUserID   uuid.UUID  `gorm:"type:uuid;not null;column:to_user_id"`
	CodeHash   []byte     `gorm:"not null;column:code_hash"`
	ExpiresAt  time.Time  `gorm:"not null;column:expires_at"`
	UsedAt     *time.Time `gorm:"column:used_at"`
	CreatedAt  time.Time  `gorm:"not null;column:created_at"`
}

func (OwnershipTransferOTPModel) TableName() string { return "ownership_transfer_otps" }
