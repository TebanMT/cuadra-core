//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// GymModel is the GORM mapping for `gyms`. Domain has no GORM tags — see
// repositories/gym_mapper.go for the bidirectional translation.
type GymModel struct {
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID     uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version   int        `gorm:"not null;default:1;column:version"`
	CreatedAt time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt *time.Time `gorm:"column:deleted_at"`

	Name     *string `gorm:"column:name"`
	City     *string `gorm:"column:city"`
	WhatsApp *string `gorm:"column:whatsapp"`
	Country  string  `gorm:"type:char(2);not null;column:country"`
	Timezone string  `gorm:"not null;column:timezone"`

	RFC           *string `gorm:"column:rfc"`
	RazonSocial   *string `gorm:"column:razon_social"`
	CodigoPostal  *string `gorm:"column:codigo_postal"`
	RegimenFiscal *string `gorm:"column:regimen_fiscal"`

	LogoURL        *string `gorm:"column:logo_url"`
	PrimaryColor   *string `gorm:"column:primary_color"`
	SecondaryColor *string `gorm:"column:secondary_color"`

	PaymentMethods string  `gorm:"type:jsonb;not null;default:'[]';column:payment_methods"`
	OpenTime       *string `gorm:"type:time;column:open_time"`
	CloseTime      *string `gorm:"type:time;column:close_time"`

	SubscriptionPlan   string     `gorm:"not null;default:'trial';column:subscription_plan"`
	TrialEndsAt        *time.Time `gorm:"column:trial_ends_at"`
	SubscriptionEndsAt *time.Time `gorm:"column:subscription_ends_at"`
	SubscriptionStatus string     `gorm:"not null;default:'active';column:subscription_status"`
	SetupCompletedAt   *time.Time `gorm:"column:setup_completed_at"`

	WhatsAppBusinessPhone    *string    `gorm:"column:whatsapp_business_phone"`
	WhatsAppBusinessTokenEnc []byte     `gorm:"column:whatsapp_business_token_enc"`
	WhatsAppConnectedAt      *time.Time `gorm:"column:whatsapp_connected_at"`

	KioskSettings  string `gorm:"type:jsonb;not null;column:kiosk_settings"`
	ChargeSettings string `gorm:"type:jsonb;not null;default:'{}';column:charge_settings"`
}

func (GymModel) TableName() string { return "gyms" }
