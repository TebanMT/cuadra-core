//go:build server

package repositories

import (
	"github.com/google/uuid"

	eventDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/event"
	"github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type WhatsAppEventPostgresRepository struct{}

func NewWhatsAppEventPostgresRepository() *WhatsAppEventPostgresRepository {
	return &WhatsAppEventPostgresRepository{}
}

func (r *WhatsAppEventPostgresRepository) Create(tx sharedDomain.Transaction, e *eventDomain.WhatsAppEvent) (*eventDomain.WhatsAppEvent, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := models.WhatsAppEventModel{
		ID:                e.ID,
		GymID:             e.GymID,
		NotificationID:    e.NotificationID,
		ProviderMessageID: e.ProviderMessageID,
		EventType:         e.EventType,
		Status:            e.Status,
		ErrorCode:         e.ErrorCode,
		ErrorMessage:      e.ErrorMessage,
		RawPayload:        e.RawPayload,
		ReceivedAt:        e.ReceivedAt,
	}
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	e.ID = row.ID
	return e, nil
}

func (r *WhatsAppEventPostgresRepository) ListByNotification(tx sharedDomain.Transaction, notificationID uuid.UUID) ([]*eventDomain.WhatsAppEvent, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.WhatsAppEventModel
	if err := gormTx.Where("notification_id = ?", notificationID).Order("received_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*eventDomain.WhatsAppEvent, len(rows))
	for i, m := range rows {
		out[i] = &eventDomain.WhatsAppEvent{
			ID:                m.ID,
			GymID:             m.GymID,
			NotificationID:    m.NotificationID,
			ProviderMessageID: m.ProviderMessageID,
			EventType:         m.EventType,
			Status:            m.Status,
			ErrorCode:         m.ErrorCode,
			ErrorMessage:      m.ErrorMessage,
			RawPayload:        m.RawPayload,
			ReceivedAt:        m.ReceivedAt,
		}
	}
	return out, nil
}
