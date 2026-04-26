//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	"github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type NotificationPostgresRepository struct{}

func NewNotificationPostgresRepository() *NotificationPostgresRepository {
	return &NotificationPostgresRepository{}
}

func (r *NotificationPostgresRepository) Create(tx sharedDomain.Transaction, n *notiDomain.Notification) (*notiDomain.Notification, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row, err := notificationToModel(n)
	if err != nil {
		return nil, err
	}
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return notificationFromModel(&row)
}

func (r *NotificationPostgresRepository) Update(tx sharedDomain.Transaction, n *notiDomain.Notification) (*notiDomain.Notification, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	n.UpdatedAt = time.Now().UTC()
	updates := map[string]any{
		"version":             n.Version,
		"updated_at":          n.UpdatedAt,
		"status":              n.Status,
		"sent_at":             n.SentAt,
		"failed_at":           n.FailedAt,
		"error_message":       n.ErrorMessage,
		"retry_count":         n.RetryCount,
		"scheduled_for":       n.ScheduledFor,
		"provider_message_id": n.ProviderMessageID,
	}
	if err := gormTx.Model(&models.NotificationModel{}).Where("id = ?", n.ID).Updates(updates).Error; err != nil {
		return nil, err
	}
	return n, nil
}

func (r *NotificationPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*notiDomain.Notification, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.NotificationModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(notiErrors.ErrNotificationNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return notificationFromModel(&row)
}

func (r *NotificationPostgresRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*notiDomain.Notification, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.NotificationModel
	err := gormTx.Where("gym_id = ? AND idempotency_key = ? AND deleted_at IS NULL", gymID, key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return notificationFromModel(&row)
}

func (r *NotificationPostgresRepository) GetByProviderMessageID(tx sharedDomain.Transaction, providerMessageID string) (*notiDomain.Notification, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.NotificationModel
	err := gormTx.Where("provider_message_id = ? AND deleted_at IS NULL", providerMessageID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return notificationFromModel(&row)
}

func (r *NotificationPostgresRepository) LeasePending(tx sharedDomain.Transaction, now time.Time, limit int) ([]*notiDomain.Notification, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 {
		limit = 50
	}
	var rows []models.NotificationModel
	err := gormTx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where("status = ? AND deleted_at IS NULL AND scheduled_for <= ?", notiDomain.StatusPending, now).
		Order("scheduled_for ASC").
		Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]*notiDomain.Notification, 0, len(rows))
	for i := range rows {
		n, err := notificationFromModel(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func (r *NotificationPostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, statusFilter string, page, pageSize int) ([]*notiDomain.Notification, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	base := gormTx.Model(&models.NotificationModel{}).
		Where("gym_id = ? AND deleted_at IS NULL", gymID)
	if statusFilter != "" {
		base = base.Where("status = ?", statusFilter)
	}
	var total int64
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.NotificationModel
	if err := base.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*notiDomain.Notification, 0, len(rows))
	for i := range rows {
		n, err := notificationFromModel(&rows[i])
		if err != nil {
			return nil, 0, err
		}
		out = append(out, n)
	}
	return out, int(total), nil
}

func notificationToModel(n *notiDomain.Notification) (models.NotificationModel, error) {
	payload, err := json.Marshal(n.Payload)
	if err != nil {
		return models.NotificationModel{}, err
	}
	return models.NotificationModel{
		ID:                n.ID,
		GymID:             n.GymID,
		Version:           n.Version,
		CreatedAt:         n.CreatedAt,
		UpdatedAt:         n.UpdatedAt,
		DeletedAt:         n.DeletedAt,
		Channel:           n.Channel,
		TemplateKey:       n.TemplateKey,
		RecipientType:     n.RecipientType,
		RecipientID:       n.RecipientID,
		RecipientAddress:  n.RecipientAddress,
		Payload:           payload,
		Status:            n.Status,
		SentAt:            n.SentAt,
		FailedAt:          n.FailedAt,
		ErrorMessage:      n.ErrorMessage,
		RetryCount:        n.RetryCount,
		ScheduledFor:      n.ScheduledFor,
		IdempotencyKey:    n.IdempotencyKey,
		ProviderMessageID: n.ProviderMessageID,
	}, nil
}

func notificationFromModel(m *models.NotificationModel) (*notiDomain.Notification, error) {
	payload := map[string]string{}
	if len(m.Payload) > 0 {
		if err := json.Unmarshal(m.Payload, &payload); err != nil {
			return nil, err
		}
	}
	return &notiDomain.Notification{
		ID:                m.ID,
		GymID:             m.GymID,
		Version:           m.Version,
		Channel:           m.Channel,
		TemplateKey:       m.TemplateKey,
		RecipientType:     m.RecipientType,
		RecipientID:       m.RecipientID,
		RecipientAddress:  m.RecipientAddress,
		Payload:           payload,
		Status:            m.Status,
		SentAt:            m.SentAt,
		FailedAt:          m.FailedAt,
		ErrorMessage:      m.ErrorMessage,
		RetryCount:        m.RetryCount,
		ScheduledFor:      m.ScheduledFor,
		IdempotencyKey:    m.IdempotencyKey,
		ProviderMessageID: m.ProviderMessageID,
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
		DeletedAt:         m.DeletedAt,
	}, nil
}
