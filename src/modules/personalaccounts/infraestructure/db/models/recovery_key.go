//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

type RecoveryKeyModel struct {
	ID                 uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	AccountID          uuid.UUID  `gorm:"column:account_id;type:uuid"`
	SecretVerifier     []byte     `gorm:"column:secret_verifier"`
	VerifierKeyVersion string     `gorm:"column:verifier_key_version"`
	Generation         int64      `gorm:"column:generation"`
	CreatedAt          time.Time  `gorm:"column:created_at;autoCreateTime:false"`
	PreparedAt         *time.Time `gorm:"column:prepared_at"`
	ConsumedAt         *time.Time `gorm:"column:consumed_at"`
	RevokedAt          *time.Time `gorm:"column:revoked_at"`
}

func (RecoveryKeyModel) TableName() string { return "mancuerna.recovery_keys" }
