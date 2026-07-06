package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// EnqueueReceiptInput is the data the BillingEventSubscriber forwards from
// PaymentCompletedEvent. The use case is responsible for the rest:
// fetching the gym (to render gym_name + check whatsapp_connected) and
// member (for first name + phone).
type EnqueueReceiptInput struct {
	GymID     uuid.UUID
	PaymentID uuid.UUID
	MemberID  uuid.UUID
	Concept   string
	Amount    float64
	Folio     string
	// MembershipTypeName + NewExpiry: nombre real del plan y vigencia real
	// (sólo en pagos de membresía; vacío/nil en ventas de producto). El
	// recibo antes hardcodeaba "Mensual" y aproximaba hoy+30d.
	MembershipTypeName string
	NewExpiry          *time.Time
	// Resend — true en el reenvío manual (UC-020). Cambia la semántica de
	// idempotencia: el camino automático dedupea por pago para siempre
	// (un cobro = un recibo); el reenvío respeta un envío EN VUELO (no
	// duplica un pending) pero sí encola de nuevo cuando el último intento
	// ya salió (sent) o murió sin llegar (held/failed).
	Resend bool
}

// EnqueueReceiptOutput is mostly for tests and the manual-resend path —
// the automatic caller is the event bus, which ignores it.
type EnqueueReceiptOutput struct {
	Skipped       bool
	SkippedReason string
	// AlreadyPending — el recibo del pago sigue en la cola sin despachar;
	// en modo Resend NO se duplica y se reporta para que el operador sepa
	// que ya va en camino.
	AlreadyPending bool
	NotificationID *uuid.UUID
}

// EnqueueReceipt is UC-039. It writes one row to notification_queue when the
// member has a phone — otherwise the row is skipped silently (DA-38.3 cola si
// no conectado does NOT apply to receipts: receipts are best-effort, the
// operator can use UC-020 manual). Per ADR-009 there is no WhatsApp-connection
// gate: the dispatcher resolves the sender by tier (Tinta master for Standard,
// the gym's own number for connected Plus).
type EnqueueReceipt struct {
	Notifications notiRepo.NotificationRepository
	Gyms          gymRepo.GymRepository
	Members       memRepo.MemberRepository
	// Templates — toggle del gym (Ajustes → Mensajes). Si el dueño
	// desactivó el template del comprobante, no se encola nada. Nil = sin
	// gate (tests viejos); los mains lo cablean siempre.
	Templates notiRepo.TemplateOverrideRepository
	UoW       sharedDomain.UnitOfWork
}

func NewEnqueueReceipt(
	notifications notiRepo.NotificationRepository,
	gyms gymRepo.GymRepository,
	members memRepo.MemberRepository,
	templates notiRepo.TemplateOverrideRepository,
	uow sharedDomain.UnitOfWork,
) *EnqueueReceipt {
	return &EnqueueReceipt{
		Notifications: notifications, Gyms: gyms, Members: members,
		Templates: templates, UoW: uow,
	}
}

func (uc *EnqueueReceipt) Execute(ctx context.Context, in EnqueueReceiptInput) (*EnqueueReceiptOutput, error) {
	now := time.Now().UTC()
	out := EnqueueReceiptOutput{}

	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		gym, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}

		// Toggle del gym (Ajustes → Mensajes): con el template del
		// comprobante desactivado no se encola NADA — ni en el cobro
		// automático ni en el reenvío manual. El gate del dispatcher cloud
		// ya lo bloqueaba al despachar ("template deshabilitado por el
		// gym"), pero eso llegaba tarde para el operador: el botón "Enviar
		// por WhatsApp" cantaba "en camino" y el envío moría después en
		// silencio. Chequear aquí (el sidecar tiene la copia local del
		// toggle, que es donde el dueño lo edita) da la verdad al instante
		// y evita llenar la bitácora de filas failed. Se chequea antes que
		// el teléfono a propósito: "la función está apagada" domina sobre
		// "a este socio le falta teléfono".
		templateKey := receiptTemplateForConcept(in.Concept)
		if uc.Templates != nil {
			override, err := uc.Templates.GetByGymAndKey(tx, in.GymID, templateKey)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if override != nil && !override.Enabled {
				out.Skipped = true
				out.SkippedReason = "template_disabled"
				return nil
			}
		}

		member, err := uc.Members.GetByID(tx, in.MemberID)
		if err != nil {
			return err
		}
		if member.GymID != in.GymID {
			out.Skipped = true
			out.SkippedReason = "cross_gym"
			return nil
		}
		if strings.TrimSpace(member.Phone) == "" {
			out.Skipped = true
			out.SkippedReason = "no_member_phone"
			return nil
		}

		gymName := ""
		if gym.Name != nil {
			gymName = *gym.Name
		}
		vars := map[string]string{
			"member_first_name": firstName(member.FullName),
			"amount":            fmt.Sprintf("%.2f", in.Amount),
			"gym_name":          gymName,
			// payment_id NO es variable del template (Twilio lo ignora vía
			// contentVariablesJSON); lo lleva el dispatcher del cloud para
			// generar el PDF del comprobante, subirlo a R2 público y meter su
			// URL como {receipt_url} ({{6}} membresía / {{4}} producto).
			"payment_id": in.PaymentID.String(),
		}
		// Membresía: tipo real ("Membresía <plan>") + vigencia real. En
		// ventas de producto estas vars no aplican (el template de producto
		// no las tiene) y se omiten.
		if in.MembershipTypeName != "" {
			vars["membership_type"] = "Membresía " + in.MembershipTypeName
		}
		if in.NewExpiry != nil {
			vars["expiry_date"] = formatSpanishDate(*in.NewExpiry)
		}

		// Idempotency: one receipt per payment. If this fires twice (e.g. a
		// bus replay) the unique index keeps us honest. El reenvío manual
		// NO crea filas nuevas: re-arma esta misma (una fila por pago,
		// siempre) — así los reenvíos repetidos, p.ej. hechos offline, no
		// se apilan en la cola ni le llegan N veces al socio al volver el
		// internet.
		idempKey := fmt.Sprintf("receipt:%s", in.PaymentID.String())
		existing, err := uc.Notifications.GetByIdempotencyKey(tx, in.GymID, idempKey)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if existing != nil {
			id := existing.ID
			out.NotificationID = &id
			if !in.Resend {
				// Camino automático: un cobro = un recibo, para siempre.
				return nil
			}
			if existing.Status == notiDomain.StatusPending {
				// Sigue en vuelo — duplicarlo mandaría dos WhatsApps (y
				// doble costo del número maestro). Se reporta al operador.
				out.AlreadyPending = true
				return nil
			}
			// sent/held/failed/cancelled → el operador quiere OTRO envío
			// real: re-arm de la misma fila (patrón de RetryNotification)
			// con teléfono y vars de HOY.
			existing.RearmForResend(member.Phone, vars, now)
			if _, err := uc.Notifications.Update(tx, existing); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			return nil
		}

		n, err := notiDomain.New(
			uuid.New(),
			in.GymID,
			member.ID,
			notiDomain.ChannelWhatsApp,
			templateKey,
			notiDomain.RecipientMember,
			member.Phone,
			vars,
			now, now,
			&idempKey,
		)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		saved, err := uc.Notifications.Create(tx, n)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		id := saved.ID
		out.NotificationID = &id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func receiptTemplateForConcept(concept string) string {
	switch concept {
	case "membership", "balance_settlement":
		return "receipt_membership"
	case "product":
		return "receipt_product"
	default:
		return "receipt_membership"
	}
}

// firstName grabs the first whitespace-delimited token, lower-noise
// fallback if FullName is empty.
func firstName(full string) string {
	full = strings.TrimSpace(full)
	if full == "" {
		return "amig@"
	}
	if i := strings.IndexByte(full, ' '); i > 0 {
		return full[:i]
	}
	return full
}

// spanishMonths abrevia el mes en español. Go's time.Format NO localiza
// (Format("02 ene 2006") imprimía el literal "ene" para CUALQUIER mes — bug
// del approx anterior), así que formateamos a mano.
var spanishMonths = [...]string{
	"", "ene", "feb", "mar", "abr", "may", "jun",
	"jul", "ago", "sep", "oct", "nov", "dic",
}

// formatSpanishDate → "20 jun 2026" para la vigencia del recibo.
func formatSpanishDate(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%02d %s %d", t.Day(), spanishMonths[t.Month()], t.Year())
}

// guard — keep template package referenced in case we expand to Render here.
var _ = tplDomain.LookupDefault
