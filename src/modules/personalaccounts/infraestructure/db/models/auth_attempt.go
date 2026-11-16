//go:build server

package models

import (
	"github.com/google/uuid"
	"time"
)

// AuthAttemptModel maps the existing schema owned by migrations 059/060.
// It is not a schema for AutoMigrate and contains no plaintext credentials.
type AuthAttemptModel struct {
	ID                         uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	AccountID                  *uuid.UUID `gorm:"column:account_id;type:uuid"`
	ProposedAccountID          uuid.UUID  `gorm:"column:proposed_account_id;type:uuid"`
	CandidateDeviceID          uuid.UUID  `gorm:"column:candidate_device_id;type:uuid"`
	Nonce                      []byte     `gorm:"column:nonce"`
	CandidatePublicKey         []byte     `gorm:"column:candidate_public_key"`
	CandidatePlatform          string     `gorm:"column:candidate_platform"`
	CandidateLabel             string     `gorm:"column:candidate_label"`
	Purpose                    string     `gorm:"column:purpose"`
	DestinationE164            string     `gorm:"column:destination_e164"`
	Channel                    string     `gorm:"column:channel"`
	ProviderReference          *string    `gorm:"column:provider_reference"`
	Status                     string     `gorm:"column:status"`
	ContinuationHash           []byte     `gorm:"column:continuation_hash"`
	RequestHash                []byte     `gorm:"column:request_hash"`
	Attempts                   int        `gorm:"column:attempts"`
	Revision                   int64      `gorm:"column:revision"`
	ExpiresAt                  time.Time  `gorm:"column:expires_at"`
	VerifiedAt                 *time.Time `gorm:"column:verified_at"`
	ConsumedAt                 *time.Time `gorm:"column:consumed_at"`
	ResultReference            *uuid.UUID `gorm:"column:result_reference;type:uuid"`
	CreatedAt                  time.Time  `gorm:"column:created_at;autoCreateTime:false"`
	ProviderOperationID        *uuid.UUID `gorm:"column:provider_operation_id;type:uuid"`
	ProviderOperationHash      []byte     `gorm:"column:provider_operation_hash"`
	ProviderOperationKind      *string    `gorm:"column:provider_operation_kind"`
	ProviderOperationStartedAt *time.Time `gorm:"column:provider_operation_started_at"`
}

func (AuthAttemptModel) TableName() string { return "mancuerna.auth_challenges" }
