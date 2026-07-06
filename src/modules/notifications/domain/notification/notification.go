// Package notification holds the Notification aggregate (a row in
// `notification_queue`). The aggregate represents one outbound message:
// pending → sent / failed / cancelled. Retries bump RetryCount and reset
// to pending on transient failure.
package notification

import (
	"strings"
	"time"

	"github.com/google/uuid"

	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
)

// Channel mirrors chk_notification_queue_channel.
const (
	ChannelWhatsApp = "whatsapp"
	ChannelEmail    = "email"
	ChannelInApp    = "in_app"
)

// Status mirrors chk_notification_queue_status.
const (
	StatusPending   = "pending"
	StatusSent      = "sent"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	// StatusHeld — el evento ocurrió hace más que el TTL del template (gym
	// estuvo mucho tiempo sin internet), así que NO se envía tarde. No es un
	// fallo ni una cancelación: es un mensaje retenido, recuperable. Fase 1 de
	// supresión de stale; la Fase 2 (Plus) agrega la UI para enviar/descartar.
	StatusHeld = "held"
)

// RecipientType mirrors chk_notification_queue_recipient_type.
const (
	RecipientMember = "member"
	RecipientUser   = "user"
)

// Notification is one row in `notification_queue`. The aggregate is
// append-only after creation except for the lifecycle fields (Status,
// SentAt, FailedAt, ErrorMessage, RetryCount, ProviderMessageID).
type Notification struct {
	ID                uuid.UUID
	GymID             uuid.UUID
	Version           int
	Channel           string
	TemplateKey       string
	RecipientType     string
	RecipientID       uuid.UUID
	RecipientAddress  string
	Payload           map[string]string
	Status            string
	SentAt            *time.Time
	FailedAt          *time.Time
	ErrorMessage      *string
	RetryCount        int
	ScheduledFor      time.Time
	IdempotencyKey    *string
	ProviderMessageID *string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
}

// New builds a pending Notification. `payload` is the ready-to-substitute
// variable map for the template; `idempotencyKey` is optional (UC-038
// scheduler uses it to guarantee one expiry reminder per day per member).
func New(
	id, gymID, recipientID uuid.UUID,
	channel, templateKey, recipientType, recipientAddress string,
	payload map[string]string,
	scheduledFor, now time.Time,
	idempotencyKey *string,
) (*Notification, error) {
	if !validChannel(channel) {
		return nil, notiErrors.ErrInvalidVariables
	}
	if !validRecipientType(recipientType) {
		return nil, notiErrors.ErrInvalidVariables
	}
	if strings.TrimSpace(templateKey) == "" {
		return nil, notiErrors.ErrTemplateNotFound
	}
	if strings.TrimSpace(recipientAddress) == "" {
		return nil, notiErrors.ErrRecipientUnreachable
	}
	if scheduledFor.IsZero() {
		scheduledFor = now
	}
	if payload == nil {
		payload = map[string]string{}
	}
	if idempotencyKey != nil {
		k := strings.TrimSpace(*idempotencyKey)
		if k == "" {
			idempotencyKey = nil
		} else {
			idempotencyKey = &k
		}
	}
	return &Notification{
		ID:               id,
		GymID:            gymID,
		Version:          1,
		Channel:          channel,
		TemplateKey:      templateKey,
		RecipientType:    recipientType,
		RecipientID:      recipientID,
		RecipientAddress: strings.TrimSpace(recipientAddress),
		Payload:          payload,
		Status:           StatusPending,
		ScheduledFor:     scheduledFor,
		IdempotencyKey:   idempotencyKey,
		CreatedAt:        now,
		UpdatedAt:        now,
	}, nil
}

// MarkSent transitions a pending notification to sent. The provider message
// id is the Twilio SID (or whatever the provider returns) — useful for
// reconciling status callbacks.
func (n *Notification) MarkSent(providerMessageID string, now time.Time) error {
	if n.Status != StatusPending {
		return notiErrors.ErrSendFailedFinal
	}
	if pm := strings.TrimSpace(providerMessageID); pm != "" {
		n.ProviderMessageID = &pm
	}
	n.Status = StatusSent
	n.SentAt = &now
	n.ErrorMessage = nil
	n.Version++
	n.UpdatedAt = now
	return nil
}

// MarkFailedRetry keeps the row pending but increments RetryCount and stores
// the last error. Used for transient failures (5xx, 429).
func (n *Notification) MarkFailedRetry(reason string, nextAttempt time.Time, now time.Time) {
	n.RetryCount++
	r := strings.TrimSpace(reason)
	if r != "" {
		n.ErrorMessage = &r
	}
	n.ScheduledFor = nextAttempt
	n.Version++
	n.UpdatedAt = now
}

// MarkFailedFinal marks the notification as definitively failed (4xx or
// retry budget exhausted).
func (n *Notification) MarkFailedFinal(reason string, now time.Time) {
	r := strings.TrimSpace(reason)
	if r == "" {
		r = "envío falló"
	}
	n.Status = StatusFailed
	n.FailedAt = &now
	n.ErrorMessage = &r
	n.Version++
	n.UpdatedAt = now
}

// ReconcileDeliveryFailure aplica un status terminal de fallo reportado por
// el provider (failed/undelivered del StatusCallback de Twilio). A diferencia
// de MarkFailedFinal, admite la transición sent→failed: el dispatcher marca
// sent en cuanto Twilio ACEPTA el mensaje, pero el veredicto de entrega de
// Meta llega después por webhook — si Meta dice que nunca se entregó, el
// status honesto es failed. Limpia SentAt para que la fila no cuente como
// entregada Y fallida a la vez en ChannelStats. No reintenta: los fallos
// terminales (número inválido, sin WhatsApp, template rechazado) no se
// arreglan reenviando; el canal de recuperación es el retry manual del dueño,
// que exige status=failed. Devuelve false (no-op) desde failed (callback
// duplicado — idempotente), cancelled y held.
func (n *Notification) ReconcileDeliveryFailure(reason string, now time.Time) bool {
	if n.Status != StatusPending && n.Status != StatusSent {
		return false
	}
	n.SentAt = nil
	n.MarkFailedFinal(reason, now)
	return true
}

// Cancel transitions to cancelled. Used by UC-022 (refund) and broadcast
// "stop" actions to keep pending rows from firing.
func (n *Notification) Cancel(now time.Time) {
	if n.Status != StatusPending {
		return
	}
	n.Status = StatusCancelled
	n.Version++
	n.UpdatedAt = now
}

// MarkHeld retiene un mensaje stale: el evento ocurrió hace más que el TTL del
// template (típicamente porque el gym estuvo mucho tiempo sin internet), así
// que NO lo enviamos tarde. No se pierde — queda en `held` y la Fase 2 (Plus)
// lo recupera o descarta. `reason` queda en ErrorMessage para contexto. Sólo
// transiciona desde pending; en cualquier otro estado es no-op.
func (n *Notification) MarkHeld(reason string, now time.Time) {
	if n.Status != StatusPending {
		return
	}
	n.Status = StatusHeld
	if r := strings.TrimSpace(reason); r != "" {
		n.ErrorMessage = &r
	}
	n.Version++
	n.UpdatedAt = now
}

// RearmForResend re-encola la MISMA fila para un reenvío manual del
// comprobante (UC-020) desde cualquier estado ya despachado o muerto
// (sent/held/failed/cancelled). Reutilizar la fila — en vez de crear otra
// con la idemp key sufijada — mantiene el invariante "una fila por pago":
// mientras esté pending, EnqueueReceipt reporta "ya en camino" y no toca
// nada, así los reenvíos repetidos (p.ej. hechos offline) no se apilan ni
// le llegan N veces al socio al volver el internet. Mismo re-arm que el
// retry manual (RetryNotification), más dos refresh propios del reenvío:
//   - CreatedAt → now: el guard anti-stale del dispatcher ancla ahí; sin
//     refrescarlo, reenviar un recibo de hace 2+ días caería a `held` al
//     instante. El reenvío es una intención NUEVA del operador, de hoy.
//   - RecipientAddress + Payload: el caso típico del reenvío es "el socio
//     tenía mal (o no tenía) el teléfono y ya se lo corrigieron" — debe
//     salir con los datos de HOY, no con los del cobro.
// RetryCount se conserva como señal histórica, igual que en el retry.
func (n *Notification) RearmForResend(recipientAddress string, payload map[string]string, now time.Time) {
	n.Status = StatusPending
	n.ScheduledFor = now
	n.CreatedAt = now
	n.SentAt = nil
	n.FailedAt = nil
	n.ErrorMessage = nil
	n.ProviderMessageID = nil
	n.RecipientAddress = strings.TrimSpace(recipientAddress)
	n.Payload = payload
	n.Version++
	n.UpdatedAt = now
}

// IsPending is a convenience for the dispatcher.
func (n *Notification) IsPending() bool { return n.Status == StatusPending }

func validChannel(c string) bool {
	switch c {
	case ChannelWhatsApp, ChannelEmail, ChannelInApp:
		return true
	}
	return false
}

func validRecipientType(t string) bool {
	switch t {
	case RecipientMember, RecipientUser:
		return true
	}
	return false
}
