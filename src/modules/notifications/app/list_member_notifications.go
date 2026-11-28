package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListMemberNotificationsInput backs GET /api/v1/members/:id/notifications:
// el historial de mensajes que el gym le ha enviado a un socio. Sin reenvío
// (cuesta dinero): es evidencia para reclamos del tipo "nunca me avisaron
// que vencía" y para las políticas tipo "si no te llega tu comprobante, el
// mes es gratis".
type ListMemberNotificationsInput struct {
	GymID    uuid.UUID
	MemberID uuid.UUID
	Page     int
	PageSize int
}

// MemberNotificationView es una fila lista para pintar: título humano,
// texto como lo recibió el socio y estado de entrega resumido.
type MemberNotificationView struct {
	ID               uuid.UUID
	Channel          string
	TemplateKey      string
	Kind             string // receipt | reminder | welcome | broadcast | other
	Title            string
	Body             string
	RecipientAddress string
	Status           string
	DeliveryState    string
	CreatedAt        time.Time
	ScheduledFor     time.Time
	SentAt           *time.Time
	DeliveredAt      *time.Time
	ReadAt           *time.Time
	FailedAt         *time.Time
	// FailureReason — explicación en español para failed/held/cancelled; el
	// error técnico del provider no le sirve al operador de recepción.
	FailureReason string
}

type ListMemberNotificationsOutput struct {
	Items    []MemberNotificationView
	Total    int
	Page     int
	PageSize int
}

type ListMemberNotifications struct {
	Notifications notiRepo.NotificationRepository
	UoW           sharedDomain.UnitOfWork
}

func NewListMemberNotifications(notifications notiRepo.NotificationRepository, uow sharedDomain.UnitOfWork) *ListMemberNotifications {
	return &ListMemberNotifications{Notifications: notifications, UoW: uow}
}

func (uc *ListMemberNotifications) Execute(ctx context.Context, in ListMemberNotificationsInput) (*ListMemberNotificationsOutput, error) {
	if in.GymID == uuid.Nil || in.MemberID == uuid.Nil {
		return nil, sharedDomain.NewValidationError(errInvalidMemberID)
	}
	page := in.Page
	if page <= 0 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	tx, err := uc.UoW.Query(ctx)
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	rows, total, err := uc.Notifications.ListByRecipient(tx, in.GymID, notiDomain.RecipientMember, in.MemberID, page, pageSize)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	items := make([]MemberNotificationView, 0, len(rows))
	for _, n := range rows {
		items = append(items, memberNotificationView(n))
	}
	return &ListMemberNotificationsOutput{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

func memberNotificationView(n *notiDomain.Notification) MemberNotificationView {
	kind, title := notificationKindTitle(n.TemplateKey)
	v := MemberNotificationView{
		ID:               n.ID,
		Channel:          n.Channel,
		TemplateKey:      n.TemplateKey,
		Kind:             kind,
		Title:            title,
		Body:             renderHistoryBody(n.TemplateKey, n.Payload),
		RecipientAddress: n.RecipientAddress,
		Status:           n.Status,
		DeliveryState:    n.DeliveryState(),
		CreatedAt:        n.CreatedAt,
		ScheduledFor:     n.ScheduledFor,
		SentAt:           n.SentAt,
		FailedAt:         n.FailedAt,
	}
	// Los acuses sólo valen sobre un envío confirmado (ver DeliveryState).
	if n.Status == notiDomain.StatusSent {
		v.DeliveredAt = n.DeliveredAt
		v.ReadAt = n.ReadAt
	}
	v.FailureReason = failureReason(n)
	return v
}

var errInvalidMemberID = errors.New("socio inválido")

// notificationKindTitle traduce la clave de template a algo que el operador
// entienda de un vistazo. Claves desconocidas (futuras) caen a "Mensaje".
func notificationKindTitle(key string) (kind, title string) {
	switch key {
	case "receipt_membership":
		return "receipt", "Comprobante de pago de membresía"
	case "receipt_product":
		return "receipt", "Comprobante de compra"
	case "expiry_reminder_3d":
		return "reminder", "Aviso de vencimiento (3 días antes)"
	case "expiry_reminder_today":
		return "reminder", "Aviso de vencimiento (vence hoy)"
	case "expiry_reminder_5d_post":
		return "reminder", "Aviso de membresía vencida"
	case "member_welcome_number":
		return "welcome", "Bienvenida y número de socio"
	case "broadcast_freeform":
		return "broadcast", "Mensaje masivo del gym"
	}
	return "other", "Mensaje"
}

// historyPlaceholders — variables que el dispatcher genera al enviar y que no
// viven en el payload guardado (el PDF / banner se sube a R2 en el momento).
var historyPlaceholders = map[string]string{
	"receipt_url":       "[PDF del comprobante adjunto]",
	"welcome_image_url": "",
}

// renderHistoryBody reconstruye el texto que recibió el socio: el cuerpo del
// template aprobado por Meta (DefaultLibrary es su espejo) con las variables
// guardadas en la fila. Nunca falla: una variable ausente se sustituye por un
// marcador en vez de esconder el mensaje entero.
func renderHistoryBody(key string, payload map[string]string) string {
	def := tplDomain.LookupDefault(key)
	if def == nil {
		if msg := strings.TrimSpace(payload["message"]); msg != "" {
			return msg
		}
		return ""
	}
	vars := make(map[string]string, len(payload)+len(historyPlaceholders))
	for k, v := range historyPlaceholders {
		vars[k] = v
	}
	for k, v := range payload {
		vars[k] = v
	}
	body := def.Body
	for {
		start := strings.IndexByte(body, '{')
		if start < 0 {
			break
		}
		end := strings.IndexByte(body[start:], '}')
		if end < 0 {
			break
		}
		name := body[start+1 : start+end]
		val, ok := vars[name]
		if !ok {
			val = "—"
		}
		body = body[:start] + val + body[start+end+1:]
	}
	return strings.Join(strings.Fields(body), " ")
}

// failureReason explica en español por qué un mensaje no llegó. Los códigos
// de Twilio más comunes en WhatsApp se mapean; el resto cae a un genérico.
func failureReason(n *notiDomain.Notification) string {
	switch n.Status {
	case notiDomain.StatusHeld:
		return "No se envió porque el aviso ya no estaba vigente cuando volvió el internet."
	case notiDomain.StatusCancelled:
		return "Se canceló antes de enviarse."
	case notiDomain.StatusFailed:
	default:
		return ""
	}
	raw := ""
	if n.ErrorMessage != nil {
		raw = strings.ToLower(*n.ErrorMessage)
	}
	switch {
	case containsAny(raw, "63024", "63003", "21211", "21614", "63001"):
		return "El número no tiene WhatsApp o no es válido."
	case containsAny(raw, "63049", "63032", "131049"):
		return "WhatsApp no entregó el mensaje (límite de mensajes promocionales para este número)."
	case containsAny(raw, "opt-out", "opt_out", "optout", "baja"):
		return "El socio pidió no recibir mensajes promocionales."
	case strings.Contains(raw, "template deshabilitado"):
		return "Este tipo de mensaje estaba desactivado en Ajustes → Mensajes."
	case strings.Contains(raw, "undelivered"):
		return "WhatsApp no pudo entregarlo al teléfono del socio."
	}
	return "No se pudo enviar."
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
