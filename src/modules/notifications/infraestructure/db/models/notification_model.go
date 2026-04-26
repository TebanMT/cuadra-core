//go:build server

package models

import (
	"time"

	"github.com/google/uuid"
)

// NotificationModel mirrors `notification_queue` (ADR-002 §3.20 + Sesión 7
// migration 002). Domain entity stays GORM-free; the mapper in repositories
// bridges them.
type NotificationModel struct {
	ID                uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID             uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version           int        `gorm:"not null;default:1;column:version"`
	CreatedAt         time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt         time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt         *time.Time `gorm:"column:deleted_at"`
	Channel           string     `gorm:"not null;column:channel"`
	TemplateKey       string     `gorm:"not null;column:template_key"`
	RecipientType     string     `gorm:"not null;column:recipient_type"`
	RecipientID       uuid.UUID  `gorm:"type:uuid;not null;column:recipient_id"`
	RecipientAddress  string     `gorm:"not null;column:recipient_address"`
	Payload           []byte     `gorm:"type:jsonb;not null;column:payload"`
	Status            string     `gorm:"not null;default:pending;column:status"`
	SentAt            *time.Time `gorm:"column:sent_at"`
	FailedAt          *time.Time `gorm:"column:failed_at"`
	ErrorMessage      *string    `gorm:"column:error_message"`
	RetryCount        int        `gorm:"not null;default:0;column:retry_count"`
	ScheduledFor      time.Time  `gorm:"not null;column:scheduled_for"`
	IdempotencyKey    *string    `gorm:"column:idempotency_key"`
	ProviderMessageID *string    `gorm:"column:provider_message_id"`
}

func (NotificationModel) TableName() string { return "notification_queue" }

// TemplateOverrideModel mirrors `notification_templates`.
type TemplateOverrideModel struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID       uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version     int        `gorm:"not null;default:1;column:version"`
	CreatedAt   time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt   time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt   *time.Time `gorm:"column:deleted_at"`
	TemplateKey string     `gorm:"not null;column:template_key"`
	Body        string     `gorm:"not null;column:body"`
	Enabled     bool       `gorm:"not null;default:true;column:enabled"`
}

func (TemplateOverrideModel) TableName() string { return "notification_templates" }

// WhatsAppEventModel mirrors `whatsapp_events`.
type WhatsAppEventModel struct {
	ID                uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID             *uuid.UUID `gorm:"type:uuid;column:gym_id"`
	NotificationID    *uuid.UUID `gorm:"type:uuid;column:notification_id"`
	ProviderMessageID string     `gorm:"not null;column:provider_message_id"`
	EventType         string     `gorm:"not null;column:event_type"`
	Status            *string    `gorm:"column:status"`
	ErrorCode         *string    `gorm:"column:error_code"`
	ErrorMessage      *string    `gorm:"column:error_message"`
	RawPayload        []byte     `gorm:"type:jsonb;not null;column:raw_payload"`
	ReceivedAt        time.Time  `gorm:"not null;column:received_at"`
}

func (WhatsAppEventModel) TableName() string { return "whatsapp_events" }
