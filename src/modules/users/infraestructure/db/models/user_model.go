//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

type UserModel struct {
	ID                 uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID              uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version            int        `gorm:"not null;default:1;column:version"`
	CreatedAt          time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt          time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt          *time.Time `gorm:"column:deleted_at"`
	Email              string     `gorm:"not null;column:email"`
	PasswordHash       string     `gorm:"not null;column:password_hash"`
	FullName           string     `gorm:"not null;column:full_name"`
	Phone              *string    `gorm:"column:phone"`
	Role               string     `gorm:"not null;column:role"`
	Active             bool       `gorm:"not null;default:true;column:active"`
	MustChangePassword bool       `gorm:"not null;default:false;column:must_change_password"`
	LastLoginAt        *time.Time `gorm:"column:last_login_at"`
	CreatedBy          *uuid.UUID `gorm:"type:uuid;column:created_by"`
	PinHash            *string    `gorm:"column:pin_hash"`
	PinAssignedAt      *time.Time `gorm:"column:pin_assigned_at"`
}

func (UserModel) TableName() string { return "users" }

type PasswordResetTokenModel struct {
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	UserID    uuid.UUID  `gorm:"type:uuid;not null;column:user_id"`
	TokenHash []byte     `gorm:"not null;column:token_hash"`
	ExpiresAt time.Time  `gorm:"not null;column:expires_at"`
	UsedAt    *time.Time `gorm:"column:used_at"`
	CreatedAt time.Time  `gorm:"not null;column:created_at"`
}

func (PasswordResetTokenModel) TableName() string { return "password_reset_tokens" }

type RefreshTokenBlacklistModel struct {
	TokenHash []byte    `gorm:"primaryKey;column:token_hash"`
	UserID    uuid.UUID `gorm:"type:uuid;not null;column:user_id"`
	ExpiresAt time.Time `gorm:"not null;column:expires_at"`
	RevokedAt time.Time `gorm:"not null;column:revoked_at"`
}

func (RefreshTokenBlacklistModel) TableName() string { return "refresh_token_blacklist" }
