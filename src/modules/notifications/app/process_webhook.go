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
	"github.com/cuadra/cuadra-core/src/shared/phone"
)

// Intención de opt-out detectada en un mensaje entrante del socio.
const (
	intentNone = iota
	intentStop // baja: STOP / BAJA / CANCELAR / ALTO / UNSUBSCRIBE
	intentStart
)

// normalizeInboundPhone limpia el "From" de Twilio ("whatsapp:+52…") y lo
// normaliza al formato canónico (igual que los teléfonos de socios), para que
// el match en el dispatcher sea consistente.
func normalizeInboundPhone(from string) string {
	from = strings.TrimSpace(from)
	from = strings.TrimPrefix(from, "whatsapp:")
	return phone.Normalize(from)
}

// optOutIntent clasifica el texto del socio. Sólo mira la PRIMERA palabra para
// no dar falsos positivos ("trabaja" no es "baja"). Case-insensitive.
func optOutIntent(body string) int {
	b := strings.ToUpper(strings.TrimSpace(body))
	if b == "" {
		return intentNone
	}
	first := b
	if i := strings.IndexAny(b, " \t\n.,;:!"); i >= 0 {
		first = b[:i]
	}
	switch first {
	case "STOP", "BAJA", "CANCELAR", "ALTO", "UNSUBSCRIBE", "BAJAR":
		return intentStop
	case "ALTA", "START", "SUSCRIBIR", "SUBSCRIBE":
		return intentStart
	default:
		return intentNone
	}
}

// ProcessWebhookInput is the structured slice of a Twilio StatusCallback /
// IncomingMessage payload that the controller hands the use case after
// validating the signature.
type ProcessWebhookInput struct {
	EventType         string // "status" or "incoming"
	ProviderMessageID string
	Status            string
	ErrorCode         string
	ErrorMessage      string
	// Inbound (EventTypeIncoming): teléfono y texto del socio. Usados para
	// detectar STOP/BAJA → opt-out de marketing. FromPhone llega crudo de
	// Twilio ("whatsapp:+52…"); el use case lo normaliza.
	FromPhone  string
	Body       string
	RawPayload []byte
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
	// OptOut (opcional; nil = sin manejo de opt-out, p.ej. en tests). Cuando
	// el socio responde STOP/BAJA registramos su teléfono aquí; START/ALTA lo
	// quita. Se cablea sólo en cmd/server (cloud).
	OptOut notiRepo.OptOutRepository
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

		// Opt-out de marketing: si el socio respondió STOP/BAJA registramos su
		// teléfono; START/ALTA lo da de baja del opt-out (re-suscribe). Aplica
		// sólo a mensajes entrantes y cuando el repo está cableado (cloud).
		if uc.OptOut != nil && in.EventType == eventDomain.EventTypeIncoming {
			if ph := normalizeInboundPhone(in.FromPhone); ph != "" {
				switch optOutIntent(in.Body) {
				case intentStop:
					if err := uc.OptOut.SetOptedOut(tx, ph, now); err != nil {
						return sharedDomain.NewUnexpectedError(err)
					}
				case intentStart:
					if err := uc.OptOut.ClearOptOut(tx, ph); err != nil {
						return sharedDomain.NewUnexpectedError(err)
					}
				}
			}
		}

		// Status reconciliation: mark our row failed when Meta says so.
		// Cubre pending Y sent — el dispatcher marca sent en cuanto Twilio
		// acepta el mensaje, pero el failed/undelivered terminal llega después
		// por este webhook; sin la transición sent→failed la noti quedaba
		// "sent" para siempre aunque Meta nunca la entregó (invisible para la
		// persecución por pago y sin retry manual posible). No dispara retry
		// automático: corregir el status deja la fila elegible para el retry
		// manual del dueño (RetryNotification exige failed).
		if notif != nil && in.EventType == eventDomain.EventTypeStatus && eventDomain.IsTerminalFailure(in.Status) {
			reason := strings.TrimSpace(in.ErrorMessage)
			if reason == "" {
				reason = "twilio: " + in.Status
				if ec := strings.TrimSpace(in.ErrorCode); ec != "" {
					reason += " (error " + ec + ")"
				}
			}
			if notif.ReconcileDeliveryFailure(reason, now) {
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
