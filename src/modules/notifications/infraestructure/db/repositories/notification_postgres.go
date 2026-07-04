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
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
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
	if err := emitNotificationToSync(gormTx, n); err != nil {
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
	if err := emitNotificationToSync(gormTx, n); err != nil {
		return nil, err
	}
	return n, nil
}

// emitNotificationToSync espeja notification_queue en sync_entities — mismo
// patrón que emitPromotionToSync. Los cambios de status cloud-side (MarkSent
// del dispatcher, pending→failed del webhook, sent→failed de la
// reconciliación de fallos terminales de Twilio) sólo llegan al sidecar si
// sync_entities bumpea version + server_updated_at; sin este mirror el
// desktop muestra el status stale para siempre.
//
// Upsert con payload COMPLETO (no delta): las filas creadas cloud-side
// (expiry reminders del scheduler, re-encolados del projector como
// enqueueWelcomeRenotify) no existen en sync_entities, así que un UPDATE-only
// las dejaría invisibles — el ON CONFLICT las da de alta al primer toque.
//
// Timestamps en epoch-ms: el shape que ApplyPullChange aterriza en el SQLite
// del sidecar (simétrico con enqueueNotification del repo SQLite).
// idempotency_key y provider_message_id viajan por esa misma simetría, pero
// NO están en tables.go Columns → el apply del sidecar los descarta a
// propósito (provider_message_id es correlación cloud-only del StatusCallback
// de Twilio; ver el comentario en SyncedTables).
func emitNotificationToSync(g *gorm.DB, n *notiDomain.Notification) error {
	msOrNil := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return t.UTC().UnixMilli()
	}
	payload, err := json.Marshal(map[string]any{
		"id":                  n.ID.String(),
		"gym_id":              n.GymID.String(),
		"version":             n.Version,
		"created_at":          n.CreatedAt.UTC().UnixMilli(),
		"updated_at":          n.UpdatedAt.UTC().UnixMilli(),
		"deleted_at":          msOrNil(n.DeletedAt),
		"channel":             n.Channel,
		"template_key":        n.TemplateKey,
		"recipient_type":      n.RecipientType,
		"recipient_id":        n.RecipientID.String(),
		"recipient_address":   n.RecipientAddress,
		"payload":             n.Payload,
		"status":              n.Status,
		"sent_at":             msOrNil(n.SentAt),
		"failed_at":           msOrNil(n.FailedAt),
		"error_message":       n.ErrorMessage,
		"retry_count":         n.RetryCount,
		"scheduled_for":       n.ScheduledFor.UTC().UnixMilli(),
		"idempotency_key":     n.IdempotencyKey,
		"provider_message_id": n.ProviderMessageID,
	})
	if err != nil {
		return err
	}
	var deletedAt any
	if n.DeletedAt != nil {
		deletedAt = *n.DeletedAt
	}
	return g.Exec(`
		INSERT INTO sync_entities
			(gym_id, entity_type, entity_id, version, payload, server_updated_at, deleted_at)
		VALUES (?, 'notification_queue', ?, ?, ?::jsonb, NOW(), ?)
		ON CONFLICT (gym_id, entity_type, entity_id) DO UPDATE SET
			version = EXCLUDED.version,
			payload = EXCLUDED.payload,
			server_updated_at = NOW(),
			deleted_at = EXCLUDED.deleted_at`,
		n.GymID, n.ID, n.Version, string(payload), deletedAt,
	).Error
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

// ChannelStats agrega el conteo de notificaciones del canal indicado en el
// rango (since, now]. Sent = total enviadas (incluye pending/failed que sí
// salieron de la cola); Delivered = las que tienen sent_at; Failed = las que
// tienen failed_at. Pending no se reporta — el FE no lo muestra.
//
// Excluimos soft-deletes para que el dueño no vea contadores inflados por
// rows reseteadas. La query single-pass con FILTER es barata aún con
// notification_queue creciendo (índice en gym_id+channel+created_at lo cubre).
func (r *NotificationPostgresRepository) ChannelStats(tx sharedDomain.Transaction, gymID uuid.UUID, channel string, since time.Time) (notiRepo.NotificationStats, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row struct {
		Sent      int64
		Delivered int64
		Failed    int64
	}
	err := gormTx.Raw(`
		SELECT
			COUNT(*) AS sent,
			COUNT(*) FILTER (WHERE sent_at IS NOT NULL) AS delivered,
			COUNT(*) FILTER (WHERE failed_at IS NOT NULL) AS failed
		FROM notification_queue
		WHERE gym_id = ? AND channel = ?
		  AND created_at >= ?
		  AND deleted_at IS NULL
		  AND status <> 'held'
	`, gymID, channel, since.UTC()).Scan(&row).Error
	if err != nil {
		return notiRepo.NotificationStats{}, err
	}
	return notiRepo.NotificationStats{
		Sent:      int(row.Sent),
		Delivered: int(row.Delivered),
		Failed:    int(row.Failed),
	}, nil
}

// LastError devuelve el último error_message del canal — el FE lo muestra
// como banner debajo de stats si no es nil. Ignoramos rows sin failed_at
// (status=failed pero retry_count<max no llena failed_at todavía).
func (r *NotificationPostgresRepository) LastError(tx sharedDomain.Transaction, gymID uuid.UUID, channel string) (string, *time.Time, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row struct {
		ErrorMessage *string
		FailedAt     *time.Time
	}
	err := gormTx.Raw(`
		SELECT error_message, failed_at
		FROM notification_queue
		WHERE gym_id = ? AND channel = ?
		  AND failed_at IS NOT NULL
		  AND deleted_at IS NULL
		ORDER BY failed_at DESC
		LIMIT 1
	`, gymID, channel).Scan(&row).Error
	if err != nil {
		return "", nil, err
	}
	if row.ErrorMessage == nil {
		return "", row.FailedAt, nil
	}
	return *row.ErrorMessage, row.FailedAt, nil
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
