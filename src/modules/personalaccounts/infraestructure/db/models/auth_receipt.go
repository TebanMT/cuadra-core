//go:build server

package models

import (
	"github.com/google/uuid"
	"time"
)

type AuthReceiptModel struct {
	OperationID        uuid.UUID  `gorm:"column:operation_id;primaryKey"`
	AccountID          *uuid.UUID `gorm:"column:account_id"`
	AttemptID          *uuid.UUID `gorm:"column:attempt_id"`
	Purpose            string     `gorm:"column:purpose"`
	RequestHash        []byte     `gorm:"column:request_hash"`
	ResultReference    *uuid.UUID `gorm:"column:result_reference"`
	ResponseCiphertext []byte     `gorm:"column:response_ciphertext"`
	KeyVersion         *string    `gorm:"column:key_version"`
	ResponseExpiresAt  time.Time  `gorm:"column:response_expires_at"`
	ResultSecretHash   []byte     `gorm:"column:result_secret_hash"`
	CompletedAt        time.Time  `gorm:"column:completed_at"`
}

func (AuthReceiptModel) TableName() string { return "mancuerna.auth_receipts" }
