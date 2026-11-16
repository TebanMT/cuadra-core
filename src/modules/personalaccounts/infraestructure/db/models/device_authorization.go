//go:build server

package models

import (
	"github.com/google/uuid"
	"time"
)

type DeviceAuthorizationModel struct {
	ID                 uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	AccountID          uuid.UUID  `gorm:"column:account_id;type:uuid"`
	ChallengeID        uuid.UUID  `gorm:"column:challenge_id;type:uuid"`
	ApprovedByDeviceID *uuid.UUID `gorm:"column:approved_by_device_id;type:uuid"`
	ComparisonCode     string     `gorm:"column:comparison_code"`
	Status             string     `gorm:"column:status"`
	ExpiresAt          time.Time  `gorm:"column:expires_at"`
	DecidedAt          *time.Time `gorm:"column:decided_at"`
	CreatedAt          time.Time  `gorm:"column:created_at;autoCreateTime:false"`
}

func (DeviceAuthorizationModel) TableName() string { return "mancuerna.device_authorizations" }
func (DeviceAuthorizationModel) String() string {
	return "device authorization row (candidate redacted)"
}
func (a DeviceAuthorizationModel) GoString() string { return a.String() }

// Only the actor lookup selects ContinuationHash. It is not in a list DTO and
// exists solely to reuse the original challenge's validated state transition.
type DeviceAuthorizationProjection struct {
	DeviceAuthorizationModel
	CandidateDeviceID  uuid.UUID `gorm:"column:candidate_device_id"`
	CandidatePublicKey []byte    `gorm:"column:candidate_public_key"`
	CandidatePlatform  string    `gorm:"column:candidate_platform"`
	CandidateLabel     string    `gorm:"column:candidate_label"`
	ContinuationHash   []byte    `gorm:"column:continuation_hash"`
}

func (DeviceAuthorizationProjection) String() string {
	return "device authorization projection (binding redacted)"
}
func (a DeviceAuthorizationProjection) GoString() string { return a.String() }
