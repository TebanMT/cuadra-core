//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

type VaultModel struct {
	ID             uuid.UUID  `gorm:"column:vault_id;type:uuid;primaryKey"`
	AccountID      uuid.UUID  `gorm:"column:account_id;type:uuid"`
	WrappedDataKey []byte     `gorm:"column:wrapped_data_key"`
	KeyVersion     string     `gorm:"column:key_version"`
	CreatedAt      time.Time  `gorm:"column:created_at;autoCreateTime:false"`
	RetiredAt      *time.Time `gorm:"column:retired_at"`
}

func (VaultModel) TableName() string  { return "mancuerna.vault_keys" }
func (VaultModel) String() string     { return "personal vault row (envelope redacted)" }
func (v VaultModel) GoString() string { return v.String() }
