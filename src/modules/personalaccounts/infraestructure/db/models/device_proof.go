//go:build server

package models

import (
	"github.com/google/uuid"
	"time"
)

type DeviceProofModel struct {
	ID          uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	AccountID   uuid.UUID  `gorm:"column:account_id;type:uuid"`
	SessionID   uuid.UUID  `gorm:"column:session_id;type:uuid"`
	DeviceID    uuid.UUID  `gorm:"column:device_id;type:uuid"`
	Purpose     string     `gorm:"column:purpose"`
	Nonce       []byte     `gorm:"column:nonce"`
	RequestHash []byte     `gorm:"column:request_hash"`
	TokenHash   []byte     `gorm:"column:token_hash"`
	ExpiresAt   time.Time  `gorm:"column:expires_at"`
	VerifiedAt  *time.Time `gorm:"column:verified_at"`
	ConsumedAt  *time.Time `gorm:"column:consumed_at"`
	OperationID *uuid.UUID `gorm:"column:operation_id;type:uuid"`
}

func (DeviceProofModel) TableName() string { return "mancuerna.device_proofs" }
