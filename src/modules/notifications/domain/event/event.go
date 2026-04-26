// Package event holds the WhatsAppEvent value type — one row per Twilio
// StatusCallback or IncomingMessage we receive (ADR-007 §5).
package event

import (
	"time"

	"github.com/google/uuid"
)

// EventType identifies the kind of webhook payload we logged.
const (
	EventTypeStatus   = "status"
	EventTypeIncoming = "incoming"
)

// Status mirrors Twilio's MessageStatus values that we care about. The full
// spec is larger; we only enumerate the ones we drive UI / state from.
const (
	StatusQueued      = "queued"
	StatusSent        = "sent"
	StatusDelivered   = "delivered"
	StatusRead        = "read"
	StatusFailed      = "failed"
	StatusUndelivered = "undelivered"
)

// WhatsAppEvent is append-only. Stored raw (RawPayload as JSON) for forensic
// debugging; the structured fields above are denormalised for fast lookup.
type WhatsAppEvent struct {
	ID                uuid.UUID
	GymID             *uuid.UUID
	NotificationID    *uuid.UUID
	ProviderMessageID string
	EventType         string
	Status            *string
	ErrorCode         *string
	ErrorMessage      *string
	RawPayload        []byte
	ReceivedAt        time.Time
}

// NewStatusEvent builds a status-callback event. `gymID` may be unknown if
// the row predates the connect (defensive — Twilio shouldn't send those).
func NewStatusEvent(
	id uuid.UUID,
	gymID *uuid.UUID,
	notificationID *uuid.UUID,
	providerMessageID, status string,
	errorCode, errorMessage *string,
	rawPayload []byte,
	now time.Time,
) *WhatsAppEvent {
	s := status
	return &WhatsAppEvent{
		ID:                id,
		GymID:             gymID,
		NotificationID:    notificationID,
		ProviderMessageID: providerMessageID,
		EventType:         EventTypeStatus,
		Status:            &s,
		ErrorCode:         errorCode,
		ErrorMessage:      errorMessage,
		RawPayload:        rawPayload,
		ReceivedAt:        now,
	}
}

// NewIncomingEvent builds an incoming-message event. Status is irrelevant.
func NewIncomingEvent(
	id uuid.UUID,
	gymID *uuid.UUID,
	providerMessageID string,
	rawPayload []byte,
	now time.Time,
) *WhatsAppEvent {
	return &WhatsAppEvent{
		ID:                id,
		GymID:             gymID,
		ProviderMessageID: providerMessageID,
		EventType:         EventTypeIncoming,
		RawPayload:        rawPayload,
		ReceivedAt:        now,
	}
}

// IsTerminalFailure reports whether the status indicates we should stop
// retrying and mark the underlying notification as failed.
func IsTerminalFailure(status string) bool {
	return status == StatusFailed || status == StatusUndelivered
}

// IsTerminalSuccess reports whether the status indicates final success.
func IsTerminalSuccess(status string) bool {
	return status == StatusDelivered || status == StatusRead
}
