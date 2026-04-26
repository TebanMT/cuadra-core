//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// MemberFingerprintModel mirrors `member_fingerprints` (ADR-002 §3.8). Domain
// entity (fingerprint.MemberFingerprint) is GORM-free; mapper lives next to
// the postgres repo.
type MemberFingerprintModel struct {
	ID                uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID             uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version           int        `gorm:"not null;default:1;column:version"`
	CreatedAt         time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt         time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt         *time.Time `gorm:"column:deleted_at"`
	MemberID          uuid.UUID  `gorm:"type:uuid;not null;column:member_id"`
	TemplateEncrypted []byte     `gorm:"type:bytea;not null;column:template_encrypted"`
	TemplateFormat    string     `gorm:"not null;default:dp_uareu;column:template_format"`
	QualityScore      *int       `gorm:"column:quality_score"`
	RegisteredBy      uuid.UUID  `gorm:"type:uuid;not null;column:registered_by"`
}

func (MemberFingerprintModel) TableName() string { return "member_fingerprints" }
