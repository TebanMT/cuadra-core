//go:build server

package models

import (
	"github.com/google/uuid"
	"time"
)

// AccountModel is the profile projection of the existing account table. It
// deliberately excludes phone, security_version and authentication metadata.
// Never AutoMigrate this projection; migration 059 owns the full schema.
type AccountModel struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	DisplayName string     `gorm:"column:display_name"`
	WeightUnit  string     `gorm:"column:weight_unit"`
	Revision    int64      `gorm:"column:revision"`
	Status      string     `gorm:"column:status"`
	CreatedAt   time.Time  `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;autoUpdateTime:false"`
	DeletedAt   *time.Time `gorm:"column:deleted_at"`
}

func (AccountModel) TableName() string { return "mancuerna.accounts" }
