//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type NotificationSQLiteRepository struct{}

func NewNotificationSQLiteRepository() *NotificationSQLiteRepository {
	return &NotificationSQLiteRepository{}
}

type sqliteNotificationRow struct {
	ID                string         `db:"id"`
	GymID             string         `db:"gym_id"`
	Version           int            `db:"version"`
	CreatedAt         int64          `db:"created_at"`
	UpdatedAt         int64          `db:"updated_at"`
	DeletedAt         sql.NullInt64  `db:"deleted_at"`
	SyncedAt          sql.NullInt64  `db:"synced_at"`
	Channel           string         `db:"channel"`
	TemplateKey       string         `db:"template_key"`
	RecipientType     string         `db:"recipient_type"`
	RecipientID       string         `db:"recipient_id"`
	RecipientAddress  string         `db:"recipient_address"`
	Payload           string         `db:"payload"`
	Status            string         `db:"status"`
	SentAt            sql.NullInt64  `db:"sent_at"`
	FailedAt          sql.NullInt64  `db:"failed_at"`
	ErrorMessage      sql.NullString `db:"error_message"`
	RetryCount        int            `db:"retry_count"`
	ScheduledFor      int64          `db:"scheduled_for"`
	IdempotencyKey    sql.NullString `db:"idempotency_key"`
	ProviderMessageID sql.NullString `db:"provider_message_id"`
}

func (r *NotificationSQLiteRepository) Create(tx sharedDomain.Transaction, n *notiDomain.Notification) (*notiDomain.Notification, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row, err := notificationToRow(n)
	if err != nil {
		return nil, err
	}
	const stmt = `
		INSERT INTO notification_queue (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    channel, template_key, recipient_type, recipient_id, recipient_address,
		    payload, status, sent_at, failed_at, error_message, retry_count,
		    scheduled_for, idempotency_key, provider_message_id
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :channel, :template_key, :recipient_type, :recipient_id, :recipient_address,
		    :payload, :status, :sent_at, :failed_at, :error_message, :retry_count,
		    :scheduled_for, :idempotency_key, :provider_message_id
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueNotification(stx, n); err != nil {
		return nil, err
	}
	return n, nil
}

func (r *NotificationSQLiteRepository) Update(tx sharedDomain.Transaction, n *notiDomain.Notification) (*notiDomain.Notification, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	n.UpdatedAt = time.Now().UTC()
	row, err := notificationToRow(n)
	if err != nil {
		return nil, err
	}
	const stmt = `
		UPDATE notification_queue SET
		    version = :version, updated_at = :updated_at,
		    status = :status, sent_at = :sent_at, failed_at = :failed_at,
		    error_message = :error_message, retry_count = :retry_count,
		    scheduled_for = :scheduled_for, provider_message_id = :provider_message_id
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueNotification(stx, n); err != nil {
		return nil, err
	}
	return n, nil
}

func (r *NotificationSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*notiDomain.Notification, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteNotificationRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM notification_queue WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(notiErrors.ErrNotificationNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return notificationFromRow(&row)
}

func (r *NotificationSQLiteRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*notiDomain.Notification, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteNotificationRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM notification_queue WHERE gym_id = ? AND idempotency_key = ? AND deleted_at IS NULL`,
		gymID.String(), key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return notificationFromRow(&row)
}

func (r *NotificationSQLiteRepository) GetByProviderMessageID(tx sharedDomain.Transaction, providerMessageID string) (*notiDomain.Notification, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteNotificationRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM notification_queue WHERE provider_message_id = ? AND deleted_at IS NULL`,
		providerMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return notificationFromRow(&row)
}

func (r *NotificationSQLiteRepository) LeasePending(tx sharedDomain.Transaction, now time.Time, limit int) ([]*notiDomain.Notification, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 {
		limit = 50
	}
	var rows []sqliteNotificationRow
	q := fmt.Sprintf(
		`SELECT * FROM notification_queue
		 WHERE status = ? AND deleted_at IS NULL AND scheduled_for <= ?
		 ORDER BY scheduled_for ASC
		 LIMIT %d`, limit)
	if err := stx.Select(context.Background(), &rows, q, notiDomain.StatusPending, now.UTC().UnixMilli()); err != nil {
		return nil, err
	}
	out := make([]*notiDomain.Notification, 0, len(rows))
	for i := range rows {
		n, err := notificationFromRow(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func (r *NotificationSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, statusFilter string, page, pageSize int) ([]*notiDomain.Notification, int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where := []string{"gym_id = ?", "deleted_at IS NULL"}
	args := []any{gymID.String()}
	if statusFilter != "" {
		where = append(where, "status = ?")
		args = append(args, statusFilter)
	}
	whereClause := strings.Join(where, " AND ")
	var total int
	if err := stx.Get(context.Background(), &total,
		`SELECT COUNT(*) FROM notification_queue WHERE `+whereClause, args...); err != nil {
		return nil, 0, err
	}
	q := fmt.Sprintf(
		`SELECT * FROM notification_queue WHERE %s ORDER BY created_at DESC LIMIT %d OFFSET %d`,
		whereClause, pageSize, (page-1)*pageSize)
	var rows []sqliteNotificationRow
	if err := stx.Select(context.Background(), &rows, q, args...); err != nil {
		return nil, 0, err
	}
	out := make([]*notiDomain.Notification, 0, len(rows))
	for i := range rows {
		n, err := notificationFromRow(&rows[i])
		if err != nil {
			return nil, 0, err
		}
		out = append(out, n)
	}
	return out, total, nil
}

func notificationToRow(n *notiDomain.Notification) (map[string]any, error) {
	payload, err := json.Marshal(n.Payload)
	if err != nil {
		return nil, err
	}
	row := map[string]any{
		"id":                  n.ID.String(),
		"gym_id":              n.GymID.String(),
		"version":             n.Version,
		"created_at":          n.CreatedAt.UTC().UnixMilli(),
		"updated_at":          n.UpdatedAt.UTC().UnixMilli(),
		"deleted_at":          nullInt64FromTime(n.DeletedAt),
		"channel":             n.Channel,
		"template_key":        n.TemplateKey,
		"recipient_type":      n.RecipientType,
		"recipient_id":        n.RecipientID.String(),
		"recipient_address":   n.RecipientAddress,
		"payload":             string(payload),
		"status":              n.Status,
		"sent_at":             nullInt64FromTime(n.SentAt),
		"failed_at":           nullInt64FromTime(n.FailedAt),
		"error_message":       nullStringFromPtr(n.ErrorMessage),
		"retry_count":         n.RetryCount,
		"scheduled_for":       n.ScheduledFor.UTC().UnixMilli(),
		"idempotency_key":     nullStringFromPtr(n.IdempotencyKey),
		"provider_message_id": nullStringFromPtr(n.ProviderMessageID),
	}
	return row, nil
}

func notificationFromRow(r *sqliteNotificationRow) (*notiDomain.Notification, error) {
	id, err := uuid.Parse(r.ID)
	if err != nil {
		return nil, err
	}
	gymID, err := uuid.Parse(r.GymID)
	if err != nil {
		return nil, err
	}
	recipientID, err := uuid.Parse(r.RecipientID)
	if err != nil {
		return nil, err
	}
	payload := map[string]string{}
	if r.Payload != "" {
		if err := json.Unmarshal([]byte(r.Payload), &payload); err != nil {
			return nil, err
		}
	}
	n := &notiDomain.Notification{
		ID:               id,
		GymID:            gymID,
		Version:          r.Version,
		Channel:          r.Channel,
		TemplateKey:      r.TemplateKey,
		RecipientType:    r.RecipientType,
		RecipientID:      recipientID,
		RecipientAddress: r.RecipientAddress,
		Payload:          payload,
		Status:           r.Status,
		RetryCount:       r.RetryCount,
		ScheduledFor:     time.UnixMilli(r.ScheduledFor).UTC(),
		CreatedAt:        time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:        time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		n.DeletedAt = &t
	}
	if r.SentAt.Valid {
		t := time.UnixMilli(r.SentAt.Int64).UTC()
		n.SentAt = &t
	}
	if r.FailedAt.Valid {
		t := time.UnixMilli(r.FailedAt.Int64).UTC()
		n.FailedAt = &t
	}
	if r.ErrorMessage.Valid {
		s := r.ErrorMessage.String
		n.ErrorMessage = &s
	}
	if r.IdempotencyKey.Valid {
		s := r.IdempotencyKey.String
		n.IdempotencyKey = &s
	}
	if r.ProviderMessageID.Valid {
		s := r.ProviderMessageID.String
		n.ProviderMessageID = &s
	}
	return n, nil
}

func enqueueNotification(stx *sharedDomain.SqlxTransaction, n *notiDomain.Notification) error {
	if stx.Queue == nil {
		return nil
	}
	payloadJSON, err := json.Marshal(map[string]any{
		"id":                  n.ID.String(),
		"gym_id":              n.GymID.String(),
		"version":             n.Version,
		"channel":             n.Channel,
		"template_key":        n.TemplateKey,
		"recipient_type":      n.RecipientType,
		"recipient_id":        n.RecipientID.String(),
		"recipient_address":   n.RecipientAddress,
		"payload":             n.Payload,
		"status":              n.Status,
		"retry_count":         n.RetryCount,
		"scheduled_for":       n.ScheduledFor.UTC().UnixMilli(),
		"idempotency_key":     ptrOrEmpty(n.IdempotencyKey),
		"provider_message_id": ptrOrEmpty(n.ProviderMessageID),
		"updated_at":          n.UpdatedAt.UTC().UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "notification_queue", n.ID.String(), "upsert", payloadJSON, n.Version)
}

// ChannelStats — espeja la versión Postgres pero usando los timestamps en
// milisegundos del SQLite local. Se ejecuta sobre la fila local de
// notification_queue (que el sync agent rellena vía pull desde cloud).
func (r *NotificationSQLiteRepository) ChannelStats(tx sharedDomain.Transaction, gymID uuid.UUID, channel string, since time.Time) (notiRepo.NotificationStats, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row struct {
		Sent      int64 `db:"sent"`
		Delivered int64 `db:"delivered"`
		Failed    int64 `db:"failed"`
	}
	// SQLite no soporta FILTER (WHERE …) en versiones <3.30, pero el sidecar
	// trae 3.40+. Aún así uso CASE para máxima portabilidad (simétrico con
	// el resto del repo).
	err := stx.Get(context.Background(), &row, `
		SELECT
			COUNT(*) AS sent,
			SUM(CASE WHEN sent_at IS NOT NULL THEN 1 ELSE 0 END) AS delivered,
			SUM(CASE WHEN failed_at IS NOT NULL THEN 1 ELSE 0 END) AS failed
		FROM notification_queue
		WHERE gym_id = ? AND channel = ?
		  AND created_at >= ?
		  AND deleted_at IS NULL
			  AND status <> 'held'`,
		gymID.String(), channel, since.UTC().UnixMilli(),
	)
	if err != nil {
		return notiRepo.NotificationStats{}, err
	}
	return notiRepo.NotificationStats{
		Sent:      int(row.Sent),
		Delivered: int(row.Delivered),
		Failed:    int(row.Failed),
	}, nil
}

func (r *NotificationSQLiteRepository) LastError(tx sharedDomain.Transaction, gymID uuid.UUID, channel string) (string, *time.Time, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row struct {
		ErrorMessage sql.NullString `db:"error_message"`
		FailedAt     sql.NullInt64  `db:"failed_at"`
	}
	err := stx.Get(context.Background(), &row, `
		SELECT error_message, failed_at
		FROM notification_queue
		WHERE gym_id = ? AND channel = ?
		  AND failed_at IS NOT NULL
		  AND deleted_at IS NULL
		ORDER BY failed_at DESC
		LIMIT 1`,
		gymID.String(), channel,
	)
	if err != nil {
		// sql.ErrNoRows = no hay fallidos todavía; devolvemos vacío sin error.
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, nil
		}
		return "", nil, err
	}
	var failedAt *time.Time
	if row.FailedAt.Valid {
		t := time.UnixMilli(row.FailedAt.Int64).UTC()
		failedAt = &t
	}
	if !row.ErrorMessage.Valid {
		return "", failedAt, nil
	}
	return row.ErrorMessage.String, failedAt, nil
}

func nullInt64FromTime(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UTC().UnixMilli(), Valid: true}
}

func nullStringFromPtr(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

func ptrOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
