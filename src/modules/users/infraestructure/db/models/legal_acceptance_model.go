//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// LegalAcceptanceModel — fila de legal_acceptances (migración 069).
type LegalAcceptanceModel struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey;column:id"`
	GymID      uuid.UUID `gorm:"type:uuid;not null;column:gym_id"`
	UserID     uuid.UUID `gorm:"type:uuid;not null;column:user_id"`
	Document   string    `gorm:"not null;column:document"`
	Version    string    `gorm:"not null;column:version"`
	AcceptedAt time.Time `gorm:"not null;column:accepted_at"`
	IPAddress  *string   `gorm:"column:ip_address"`
	UserAgent  *string   `gorm:"column:user_agent"`
}

func (LegalAcceptanceModel) TableName() string { return "legal_acceptances" }
