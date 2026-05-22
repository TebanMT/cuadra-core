package app

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ErrBillingNotWired se devuelve si el caller pidió cobrar el primer
// pago pero al construir el use case no se inyectaron las dependencias
// de billing (Payments + Folios). En producción ambas dependencias se
// cablean — esto sólo aparece en tests que no quieran cobrar.
var ErrBillingNotWired = errors.New("create-member: billing dependencies missing for first-payment charge")

// CreateMemberInput backs UC-012. The only required fields are GymID +
// FullName + Phone + MembershipTypeID. Optional fields use pointers (nil ==
// "not provided"). When `AllowDuplicatePhone` is false and the phone already
// exists the use case returns a BusinessError; the front-end can re-call with
// AllowDuplicatePhone=true to confirm (DA-12.3).
type CreateMemberInput struct {
	GymID               uuid.UUID
	ActorUserID         uuid.UUID
	FullName            string
	Phone               string
	Email               *string
	Birthdate           *time.Time
	PhotoURL            *string
	Notes               *string
	// Gender opcional (DA-012.7). nil o "" = no se captura; valor válido se
	// persiste tras pasar por memberDomain.ValidateGender. NO se infiere
	// del nombre.
	Gender              *string
	MembershipTypeID    uuid.UUID
	StartDate           time.Time
	AllowDuplicatePhone bool
	// ChargeFirstPayment marca el toggle "Cobrar primer pago ahora"
	// (DA-12.1). Cuando es true, el use case crea Payment + folio en la
	// misma tx que el Member/Membership y deja consistente enrollment_paid
	// / last_maintenance_paid. PaymentMethod entonces es obligatorio.
	//
	// ChargeEnrollment / ChargeMaintenance permiten al operador "saltar"
	// la cuota de inscripción o mantenimiento aunque el plan las traiga
	// (caso "promo primer año sin inscripción"). Si el plan no cobra esa
	// cuota (fee == 0 ó freq nil), el flag se ignora silenciosamente.
	ChargeFirstPayment bool
	ChargeEnrollment   bool
	ChargeMaintenance  bool
	// Si el caller resuelve un monto distinto al snapshot del plan
	// (típicamente porque la config a nivel gym cambió después de que
	// el plan se creó), puede pasarlos aquí. Si vienen <= 0, caemos
	// al snapshot del plan (mt.EnrollmentFee / mt.MaintenanceFee).
	EnrollmentAmount  float64
	MaintenanceAmount float64
	PaymentMethod     string
}

// CreateMemberOutput contains the new member's id and folio. Cuando se
// cobra el primer pago, PaymentID / PaymentFolio / PaymentTotal vienen
// poblados — el FE los puede usar para mostrar recibo o folio en pantalla.
// Pin viene poblado siempre: la inscripción auto-genera un PIN de 4
// dígitos único en el gym y lo devuelve para que el operador lo escriba
// en la credencial. Después se puede consultar / cambiar desde el perfil.
// PinDispatch refleja si la notificación de WhatsApp con el PIN se
// encoló — el FE lo usa para decidir entre "enviado a +52…" y "escríbelo
// en la credencial".
type CreateMemberOutput struct {
	MemberID     uuid.UUID
	MembershipID uuid.UUID
	Folio        string
	// ExpiryDate es nil cuando no se cobró primer pago — el membership
	// queda en pending_payment hasta el primer abono (parcial o total).
	ExpiryDate          *time.Time
	MembershipStatus    string // "active" | "pending_payment"
	PendingFirstPayment bool
	Pin                 string
	PinDispatch         WelcomePinDispatchResult
	PaymentID           *uuid.UUID
	PaymentFolio        string
	PaymentTotal        float64
}

type CreateMember struct {
	Members         memRepo.MemberRepository
	Memberships     memRepo.MembershipRepository
	MembershipTypes memRepo.MembershipTypeRepository
	// Payments + Folios sólo se usan cuando ChargeFirstPayment=true. En
	// tests fixtures que sólo crean socios sin cobro pueden pasar nil.
	Payments billingRepo.PaymentRepository
	Folios   *folioSvc.Generator
	// WelcomePin es la seam cross-BC que la notifications BC implementa
	// (ver notifications/app/EnqueueWelcomePin). Nil = no se envía
	// notificación; usado en tests que no exercisan WhatsApp.
	WelcomePin WelcomePinNotifier
	UoW        sharedDomain.UnitOfWork
	Audit      audit.Recorder
}

func NewCreateMember(members memRepo.MemberRepository, memberships memRepo.MembershipRepository,
	mtypes memRepo.MembershipTypeRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreateMember {
	return &CreateMember{
		Members:         members,
		Memberships:     memberships,
		MembershipTypes: mtypes,
		UoW:             uow,
		Audit:           recorder,
	}
}

// NewCreateMemberWithBilling extiende NewCreateMember para escenarios de
// cobro del primer pago. Si Payments o Folios son nil, el use case
// rechaza la solicitud con ErrBillingNotWired cuando ChargeFirstPayment=true.
func NewCreateMemberWithBilling(members memRepo.MemberRepository, memberships memRepo.MembershipRepository,
	mtypes memRepo.MembershipTypeRepository, payments billingRepo.PaymentRepository, folios *folioSvc.Generator,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreateMember {
	uc := NewCreateMember(members, memberships, mtypes, uow, recorder)
	uc.Payments = payments
	uc.Folios = folios
	return uc
}

// WithWelcomePinNotifier wires the WhatsApp welcome-PIN seam. Idempotent
// chaining helper for DI in cmd/server + cmd/sidecar; tests can skip
// calling it and the use case defaults to noopWelcomePinNotifier.
func (uc *CreateMember) WithWelcomePinNotifier(n WelcomePinNotifier) *CreateMember {
	uc.WelcomePin = n
	return uc
}

func (uc *CreateMember) Execute(ctx context.Context, in CreateMemberInput) (*CreateMemberOutput, error) {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if err := memberDomain.ValidateStartDate(in.StartDate, today); err != nil {
		return nil, sharedDomain.NewValidationError(err)
	}

	if in.ChargeFirstPayment {
		if uc.Payments == nil || uc.Folios == nil {
			return nil, sharedDomain.NewUnexpectedError(ErrBillingNotWired)
		}
		if in.PaymentMethod == "" {
			return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentMethodMissing)
		}
	}

	var out CreateMemberOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		// 1) Type must exist + belong to gym + be active.
		mt, err := uc.MembershipTypes.GetByID(tx, in.MembershipTypeID)
		if err != nil {
			return err
		}
		if mt.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeNotFound, "")
		}
		if !mt.Active {
			return sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeInactive, "")
		}

		// 2) Phone uniqueness — soft warning unless caller acknowledged.
		phoneNorm, err := normalizeAndValidatePhone(in.Phone)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if !in.AllowDuplicatePhone {
			exists, err := uc.Members.ExistsByGymAndPhone(tx, in.GymID, phoneNorm)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if exists {
				return sharedDomain.NewBusinessError(memErrors.ErrPhoneAlreadyExists, "")
			}
		}

		// 3) Folio.
		folio, err := uc.Members.NextFolio(tx, in.GymID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 4) Build domain Member.
		mID := uuid.New()
		m, err := memberDomain.NewMember(mID, in.GymID, folio, in.FullName, phoneNorm, in.ActorUserID, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		// Apply optional fields via ProfileUpdate (re-uses validation).
		if err := m.ApplyProfileUpdate(memberDomain.ProfileUpdate{
			Email:     in.Email,
			Birthdate: in.Birthdate,
			PhotoURL:  in.PhotoURL,
			Notes:     in.Notes,
			Gender:    in.Gender,
		}, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}

		// 4.5) Auto-asignar PIN. Históricamente "Asignar PIN" era un paso
		//      manual post-inscripción; ahora siempre arranca con uno
		//      generado (visible en el perfil del socio para que el
		//      operador lo lea cuando se lo olviden). Si la generación
		//      llegara a agotar reintentos (>9000 PINs activos en el
		//      gym, irreal en MVP) loguueamos y seguimos: el socio queda
		//      sin PIN y se puede asignar después manualmente — eso es
		//      mejor que abortar toda la inscripción.
		generated, gerr := generateUniquePin(tx, uc.Members, in.GymID, nil)
		if gerr == nil {
			hash, herr := auth.HashPassword(generated)
			if herr != nil {
				return sharedDomain.NewUnexpectedError(herr)
			}
			m.SetPin(hash, generated, now)
			out.Pin = generated
		} else {
			log.Printf("[create-member] no se pudo auto-asignar PIN (gym=%s member=%s): %v", in.GymID, m.ID, gerr)
		}

		if _, err := uc.Members.Create(tx, m); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 4.6) Encolar la notificación de WhatsApp con el PIN. La seam
		//      decide solita si saltarse (gym sin WhatsApp, sin teléfono,
		//      toggle off) — devuelve Dispatched=false con SkippedReason
		//      estable. Va en la misma tx para que el row de
		//      notification_queue no quede huérfano si después algo falla.
		if out.Pin != "" {
			notifier := uc.WelcomePin
			if notifier == nil {
				notifier = noopWelcomePinNotifier{}
			}
			res, nerr := notifier.Notify(ctx, tx, WelcomePinNotifyInput{
				GymID: in.GymID, MemberID: m.ID, Pin: out.Pin,
			}, now)
			if nerr != nil {
				return nerr
			}
			out.PinDispatch = res
		}

		// 5) Membership. Si va a haber primer pago en esta misma tx
		//    arrancamos en `active` (ese pago la activará al setearle
		//    la fecha de vigencia). Sin primer pago: `pending_payment`
		//    sin expiry — un abono futuro (parcial o completo) la
		//    activa vía Membership.Activate().
		var ms *membershipDomain.Membership
		if in.ChargeFirstPayment {
			ms = membershipDomain.New(uuid.New(), in.GymID, m.ID, mt, in.StartDate, now)
		} else {
			ms = membershipDomain.NewPendingPayment(uuid.New(), in.GymID, m.ID, mt, in.StartDate, now)
		}
		if _, err := uc.Memberships.Create(tx, ms); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 6) Audit del Member.
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "members",
			EntityID:    m.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"folio":              m.Folio,
				"membership_type_id": mt.ID,
				"membership_id":      ms.ID,
				"membership_status":  ms.Status,
				"expiry_date":        formatExpiryForAudit(ms.ExpiryDate),
				"first_payment":      in.ChargeFirstPayment,
				"pin_auto_assigned":  out.Pin != "",
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})

		// Preserve auto-asignado PIN + dispatch result (set above in
		// steps 4.5 / 4.6) — el reset de `out` mantiene el resto.
		generatedPin := out.Pin
		pinDispatch := out.PinDispatch
		out = CreateMemberOutput{
			MemberID:            m.ID,
			MembershipID:        ms.ID,
			Folio:               m.Folio,
			ExpiryDate:          ms.ExpiryDate,
			MembershipStatus:    ms.Status,
			PendingFirstPayment: in.ChargeFirstPayment,
			Pin:                 generatedPin,
			PinDispatch:         pinDispatch,
		}

		// 7) Primer pago (opcional). Mismo tx que la creación: si algo
		//    falla, todo rolledback. NO usa UC-018 (RegisterMembershipPayment)
		//    porque ese flujo renueva la membresía, duplicando la que
		//    acabamos de crear. Aquí cobramos la membresía recién creada,
		//    sin renovar.
		if in.ChargeFirstPayment {
			// Para cada cuota: si el caller mandó un monto explícito > 0,
			// lo usamos (caso: el plan se creó con fee=0 antes de prender
			// el toggle a nivel gym; el FE resuelve el monto efectivo).
			// Si no, caemos al snapshot del plan.
			enrollmentAmt := mt.EnrollmentFee
			if in.EnrollmentAmount > 0 {
				enrollmentAmt = in.EnrollmentAmount
			}
			maintenanceAmt := mt.MaintenanceFee
			if in.MaintenanceAmount > 0 {
				maintenanceAmt = in.MaintenanceAmount
			}

			subtotal := mt.Price
			chargeEnrollment := in.ChargeEnrollment && enrollmentAmt > 0
			if chargeEnrollment {
				subtotal += enrollmentAmt
			}
			chargeMaintenance := in.ChargeMaintenance && maintenanceAmt > 0
			if chargeMaintenance {
				subtotal += maintenanceAmt
			}

			folioPay, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptMembership)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			paymentID := uuid.New()
			// Sin discount al primer pago — las promos "sin inscripción"
			// se modelan deseleccionando ChargeEnrollment, no como descuento.
			p, err := paymentDomain.NewMembershipPayment(
				paymentID, in.GymID, in.ActorUserID, m.ID, folioPay,
				subtotal, 0, subtotal, 0,
				in.PaymentMethod, in.StartDate, now, nil, nil,
			)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			// Desglose para el recibo del primer pago — el plan + las
			// cuotas que el operador eligió cobrar.
			lines := []paymentDomain.BreakdownLine{
				{Label: mt.Name, Amount: mt.Price},
			}
			if chargeEnrollment {
				lines = append(lines, paymentDomain.BreakdownLine{
					Label: "Inscripción", Amount: enrollmentAmt,
				})
			}
			if chargeMaintenance {
				lines = append(lines, paymentDomain.BreakdownLine{
					Label: "Mantenimiento", Amount: maintenanceAmt,
				})
			}
			p.SetBreakdown(lines)
			if _, err := uc.Payments.Create(tx, p); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}

			memberDirty := false
			if chargeEnrollment {
				m.MarkEnrollmentPaid(now)
				memberDirty = true
			}
			if chargeMaintenance {
				m.UpdateLastMaintenance(in.StartDate, now)
				memberDirty = true
			}
			if memberDirty {
				if _, err := uc.Members.Update(tx, m); err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
			}

			_ = uc.Audit.Record(ctx, tx, audit.Entry{
				GymID:       in.GymID,
				EntityType:  "payments",
				EntityID:    p.ID,
				Action:      audit.ActionCreate,
				ActorUserID: &in.ActorUserID,
				Changes: map[string]any{
					"folio":            p.Folio,
					"member_id":        m.ID,
					"membership_id":    ms.ID,
					"amount":           p.Amount,
					"concept":          p.Concept,
					"method":           p.PaymentMethod,
					"enrollment_chrg":  chargeEnrollment,
					"maintenance_chrg": chargeMaintenance,
					"source":           "create_member",
				},
				IPAddress: audit.IPFromContext(ctx),
				UserAgent: audit.UAFromContext(ctx),
				At:        now,
			})

			out.PaymentID = &paymentID
			out.PaymentFolio = p.Folio
			out.PaymentTotal = p.Amount
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// formatExpiryForAudit renders the expiry as YYYY-MM-DD for audit log
// entries. Returns an empty string for pending_payment (sin expiry).
func formatExpiryForAudit(e *time.Time) string {
	if e == nil {
		return ""
	}
	return e.Format("2006-01-02")
}

func normalizeAndValidatePhone(raw string) (string, error) {
	v := raw
	v = trimSpaces(v)
	if !memberDomain.ValidatePhone(v) {
		return "", memErrors.ErrInvalidPhone
	}
	return v, nil
}

func trimSpaces(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '-', '(', ')':
			continue
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
