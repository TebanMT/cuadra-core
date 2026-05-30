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
	// MembershipType es el nombre del plan (e.g. "Mensual Premium") para el
	// template receipt_membership. Vacío = fallback "Mensual". Para ventas
	// de producto se puede dejar en blanco (el template no lo usa).
	MembershipType string
	// PhoneOverride, si está presente, reemplaza el teléfono del socio en la
	// notificación. Usado por el reenvío manual UC-020 "whatsapp_other".
	PhoneOverride *string
	// ForceNew, si es true, omite la verificación de idempotencia y crea
	// siempre una nueva fila. Útil para reenvíos manuales UC-020.
	ForceNew bool
}

// EnqueueReceiptOutput is mostly for tests — the caller is the event bus,
// not the user.
type EnqueueReceiptOutput struct {
	Skipped        bool
	SkippedReason  string
	NotificationID *uuid.UUID
}

// EnqueueReceipt is UC-039. It writes one row to notification_queue when
// the gym has WhatsApp connected and the member has a phone — otherwise
// the row is skipped silently (DA-38.3 cola si no conectado does NOT apply
// to receipts: receipts are best-effort, the operator can use UC-020 manual).
type EnqueueReceipt struct {
	Notifications notiRepo.NotificationRepository
	Gyms          gymRepo.GymRepository
	Members       memRepo.MemberRepository
	UoW           sharedDomain.UnitOfWork
	// AppBaseURL es la URL pública del frontend (e.g. https://app.entinta.mx
	// en cloud, http://localhost:5173 en sidecar). Se usa para construir el
	// receipt_url del template: <AppBaseURL>/payments/<id>/receipt.
	AppBaseURL string
}

func NewEnqueueReceipt(
	notifications notiRepo.NotificationRepository,
	gyms gymRepo.GymRepository,
	members memRepo.MemberRepository,
	uow sharedDomain.UnitOfWork,
	appBaseURL string,
) *EnqueueReceipt {
	return &EnqueueReceipt{Notifications: notifications, Gyms: gyms, Members: members, UoW: uow, AppBaseURL: appBaseURL}
}

func (uc *EnqueueReceipt) Execute(ctx context.Context, in EnqueueReceiptInput) (*EnqueueReceiptOutput, error) {
	now := time.Now().UTC()
	out := EnqueueReceiptOutput{}

	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		gym, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}
		if !gym.IsWhatsAppConnected() {
			out.Skipped = true
			out.SkippedReason = "whatsapp_not_connected"
			return nil
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
		// PhoneOverride permite al reenvío manual UC-020 dirigir el mensaje a
		// un número distinto del registrado en el socio.
		phone := strings.TrimSpace(member.Phone)
		if in.PhoneOverride != nil && strings.TrimSpace(*in.PhoneOverride) != "" {
			phone = strings.TrimSpace(*in.PhoneOverride)
		}
		if phone == "" {
			out.Skipped = true
			out.SkippedReason = "no_member_phone"
			return nil
		}

		templateKey := receiptTemplateForConcept(in.Concept)
		gymName := ""
		if gym.Name != nil {
			gymName = *gym.Name
		}
		// receipt_url apunta a la página pública del comprobante en el frontend.
		// Se construye desde AppBaseURL para que funcione tanto en cloud como en
		// el sidecar local.
		receiptURL := fmt.Sprintf("%s/payments/%s/receipt",
			strings.TrimRight(uc.AppBaseURL, "/"), in.PaymentID.String())

		membershipType := in.MembershipType
		if membershipType == "" {
			membershipType = "Mensual"
		}

		vars := map[string]string{
			"member_first_name": firstName(member.FullName),
			"amount":            fmt.Sprintf("%.2f", in.Amount),
			"gym_name":          gymName,
			"membership_type":   membershipType,
			"expiry_date":       receiptExpiryHint(member, now),
			"receipt_url":       receiptURL,
		}

		// Idempotency: one receipt per payment. If this fires twice (e.g. a
		// bus replay) the unique index keeps us honest.
		// ForceNew omite la verificación (reenvíos manuales UC-020).
		idempKey := fmt.Sprintf("receipt:%s", in.PaymentID.String())
		if !in.ForceNew {
			existing, err := uc.Notifications.GetByIdempotencyKey(tx, in.GymID, idempKey)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if existing != nil {
				id := existing.ID
				out.NotificationID = &id
				return nil
			}
		}

		n, err := notiDomain.New(
			uuid.New(),
			in.GymID,
			member.ID,
			notiDomain.ChannelWhatsApp,
			templateKey,
			notiDomain.RecipientMember,
			phone,
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

// receiptExpiryHint formats a "vigencia hasta" string for the receipt template.
// We don't have the post-payment Membership in scope (PaymentCompletedEvent
// doesn't carry it) so we approximate with `today + 30d`. Future iteration:
// pass NewExpiry through the event so the receipt is precise (UC-018 ya tiene
// renewed.NewMembership.ExpiryDate — se puede agregar en una sesión posterior).
func receiptExpiryHint(_ any, now time.Time) string {
	return now.Add(30 * 24 * time.Hour).Format("02 ene 2006")
}

// guard — keep template package referenced in case we expand to Render here.
var _ = tplDomain.LookupDefault
