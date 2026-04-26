package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	eventDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/event"
	notification "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ProcessWebhookInput is the structured slice of a Twilio StatusCallback /
// IncomingMessage payload that the controller hands the use case after
// validating the signature.
type ProcessWebhookInput struct {
	EventType         string // "status" or "incoming"
	ProviderMessageID string
	Status            string
	ErrorCode         string
	ErrorMessage      string
	RawPayload        []byte
}

type ProcessWebhookOutput struct {
	NotificationID *uuid.UUID
	UpdatedStatus  string
}

// ProcessWebhook persists the event + reconciles the matching notification
// row's status. Cloud-only flow.
type ProcessWebhook struct {
	Notifications notiRepo.NotificationRepository
	Events        notiRepo.WhatsAppEventRepository
	UoW           sharedDomain.UnitOfWork
}

func NewProcessWebhook(
	notifications notiRepo.NotificationRepository,
	events notiRepo.WhatsAppEventRepository,
	uow sharedDomain.UnitOfWork,
) *ProcessWebhook {
	return &ProcessWebhook{Notifications: notifications, Events: events, UoW: uow}
}

func (uc *ProcessWebhook) Execute(ctx context.Context, in ProcessWebhookInput) (*ProcessWebhookOutput, error) {
	now := time.Now().UTC()
	out := ProcessWebhookOutput{}

	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		// Look up the notification by provider_message_id (set when the
		// dispatcher fired the send). Webhook can race the DB update, so
		// not-found is non-fatal — we still log the event.
		var notif *notification.Notification
		if pm := strings.TrimSpace(in.ProviderMessageID); pm != "" {
			n, err := uc.Notifications.GetByProviderMessageID(tx, pm)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			notif = n
		}

		// Persist the event row first.
		var notifID *uuid.UUID
		var gymID *uuid.UUID
		if notif != nil {
			id := notif.ID
			notifID = &id
			gid := notif.GymID
			gymID = &gid
		}
		var ev *eventDomain.WhatsAppEvent
		switch in.EventType {
		case eventDomain.EventTypeIncoming:
			ev = eventDomain.NewIncomingEvent(uuid.New(), gymID, in.ProviderMessageID, in.RawPayload, now)
		default:
			var ec, em *string
			if v := strings.TrimSpace(in.ErrorCode); v != "" {
				ec = &v
			}
			if v := strings.TrimSpace(in.ErrorMessage); v != "" {
				em = &v
			}
			ev = eventDomain.NewStatusEvent(uuid.New(), gymID, notifID, in.ProviderMessageID, in.Status, ec, em, in.RawPayload, now)
		}
		if _, err := uc.Events.Create(tx, ev); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// Status reconciliation: mark our row failed when Meta says so.
		if notif != nil && in.EventType == eventDomain.EventTypeStatus && eventDomain.IsTerminalFailure(in.Status) {
			if notif.IsPending() {
				reason := strings.TrimSpace(in.ErrorMessage)
				if reason == "" {
					reason = "twilio: " + in.Status
				}
				notif.MarkFailedFinal(reason, now)
				if _, err := uc.Notifications.Update(tx, notif); err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				out.UpdatedStatus = notification.StatusFailed
			}
		}
		// Successful delivered/read — sent_at is already set; we just keep
		// the event log for analytics.

		if notifID != nil {
			out.NotificationID = notifID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
