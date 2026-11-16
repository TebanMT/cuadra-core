//go:build server

package models

import (
	"github.com/google/uuid"
	"time"
)

type SessionRotationModel struct {
	AccountID               uuid.UUID `gorm:"column:account_id;type:uuid;primaryKey"`
	SessionID               uuid.UUID `gorm:"column:session_id;type:uuid;primaryKey"`
	Generation              int64     `gorm:"column:generation;primaryKey"`
	OldRefreshHash          []byte    `gorm:"column:old_refresh_hash"`
	OldRevocationTicketHash []byte    `gorm:"column:old_revocation_ticket_hash"`
	OperationID             uuid.UUID `gorm:"column:operation_id;type:uuid"`
	RequestHash             []byte    `gorm:"column:request_hash"`
	ResponseCiphertext      []byte    `gorm:"column:response_ciphertext"`
	KeyVersion              *string   `gorm:"column:key_version"`
	ResponseExpiresAt       time.Time `gorm:"column:response_expires_at"`
	CreatedAt               time.Time `gorm:"column:created_at;autoCreateTime:false"`
}

func (SessionRotationModel) TableName() string { return "mancuerna.session_rotations" }
