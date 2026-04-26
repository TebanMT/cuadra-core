//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	eventDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/event"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type WhatsAppEventSQLiteRepository struct{}

func NewWhatsAppEventSQLiteRepository() *WhatsAppEventSQLiteRepository {
	return &WhatsAppEventSQLiteRepository{}
}

type sqliteWhatsAppEventRow struct {
	ID                string         `db:"id"`
	GymID             sql.NullString `db:"gym_id"`
	NotificationID    sql.NullString `db:"notification_id"`
	ProviderMessageID string         `db:"provider_message_id"`
	EventType         string         `db:"event_type"`
	Status            sql.NullString `db:"status"`
	ErrorCode         sql.NullString `db:"error_code"`
	ErrorMessage      sql.NullString `db:"error_message"`
	RawPayload        string         `db:"raw_payload"`
	ReceivedAt        int64          `db:"received_at"`
}

func (r *WhatsAppEventSQLiteRepository) Create(tx sharedDomain.Transaction, e *eventDomain.WhatsAppEvent) (*eventDomain.WhatsAppEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := map[string]any{
		"id":                  e.ID.String(),
		"gym_id":              uuidPtrToNullString(e.GymID),
		"notification_id":     uuidPtrToNullString(e.NotificationID),
		"provider_message_id": e.ProviderMessageID,
		"event_type":          e.EventType,
		"status":              strPtrToNullString(e.Status),
		"error_code":          strPtrToNullString(e.ErrorCode),
		"error_message":       strPtrToNullString(e.ErrorMessage),
		"raw_payload":         string(e.RawPayload),
		"received_at":         e.ReceivedAt.UTC().UnixMilli(),
	}
	const stmt = `
		INSERT INTO whatsapp_events (
		    id, gym_id, notification_id, provider_message_id, event_type,
		    status, error_code, error_message, raw_payload, received_at
		) VALUES (
		    :id, :gym_id, :notification_id, :provider_message_id, :event_type,
		    :status, :error_code, :error_message, :raw_payload, :received_at
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	return e, nil
}

func (r *WhatsAppEventSQLiteRepository) ListByNotification(tx sharedDomain.Transaction, notificationID uuid.UUID) ([]*eventDomain.WhatsAppEvent, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteWhatsAppEventRow
	if err := stx.Select(context.Background(), &rows,
		`SELECT * FROM whatsapp_events WHERE notification_id = ? ORDER BY received_at ASC`,
		notificationID.String()); err != nil {
		return nil, err
	}
	out := make([]*eventDomain.WhatsAppEvent, 0, len(rows))
	for i := range rows {
		e, err := whatsappEventFromRow(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func whatsappEventFromRow(r *sqliteWhatsAppEventRow) (*eventDomain.WhatsAppEvent, error) {
	id, err := uuid.Parse(r.ID)
	if err != nil {
		return nil, err
	}
	e := &eventDomain.WhatsAppEvent{
		ID:                id,
		ProviderMessageID: r.ProviderMessageID,
		EventType:         r.EventType,
		RawPayload:        []byte(r.RawPayload),
		ReceivedAt:        time.UnixMilli(r.ReceivedAt).UTC(),
	}
	if r.GymID.Valid {
		gid, err := uuid.Parse(r.GymID.String)
		if err == nil {
			e.GymID = &gid
		}
	}
	if r.NotificationID.Valid {
		nid, err := uuid.Parse(r.NotificationID.String)
		if err == nil {
			e.NotificationID = &nid
		}
	}
	if r.Status.Valid {
		s := r.Status.String
		e.Status = &s
	}
	if r.ErrorCode.Valid {
		s := r.ErrorCode.String
		e.ErrorCode = &s
	}
	if r.ErrorMessage.Valid {
		s := r.ErrorMessage.String
		e.ErrorMessage = &s
	}
	return e, nil
}

func uuidPtrToNullString(u *uuid.UUID) sql.NullString {
	if u == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: u.String(), Valid: true}
}

func strPtrToNullString(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}
