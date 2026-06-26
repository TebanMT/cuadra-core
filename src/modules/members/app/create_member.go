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
	promoApp "github.com/cuadra/cuadra-core/src/modules/promotions/app"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	phonepkg "github.com/cuadra/cuadra-core/src/shared/phone"
)

// CreateMemberPromotion es el sub-input opcional para aplicar una promo
// al primer pago. PromotionID y Code son excluyentes. CompanionMemberIDs
// sólo para kind=companion_memberships. La promo se valida + aplica
// dentro de la misma tx que la creación del socio + el primer pago — si
// algo falla, rollback completo (no queda socio sin pago ni pago sin
// socio).
type CreateMemberPromotion struct {
	PromotionID        *uuid.UUID
	Code               *string
	CompanionMemberIDs []uuid.UUID
	Notes              *string
}

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
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	FullName    string
	Phone       string
	Email       *string
	Birthdate   *time.Time
	PhotoURL    *string
	Notes       *string
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
	// Promotion: si el operador eligió aplicar una promo al primer pago,
	// viene poblada. Sólo se honra cuando ChargeFirstPayment=true (sin
	// primer pago no hay nada que descontar). Si falla la validación
	// (expirada, sobre-límite, etc.), la creación entera rolledback.
	Promotion *CreateMemberPromotion
}

// CreateMemberOutput contains the new member's id and folio. Cuando se
// cobra el primer pago, PaymentID / PaymentFolio / PaymentTotal vienen
// poblados — el FE los puede usar para mostrar recibo o folio en pantalla.
// MemberNumber viene poblado siempre (>0): la inscripción auto-genera un
// número de socio único en el gym (ADR-010) y lo devuelve para que el
// operador lo escriba en la credencial. Después se puede consultar /
// cambiar desde el perfil. Dispatch refleja si la notificación de WhatsApp
// con el número se encoló — el FE lo usa para decidir entre "enviado a
// +52…" y "escríbelo en la credencial".
type CreateMemberOutput struct {
	MemberID     uuid.UUID
	MembershipID uuid.UUID
	Folio        string
	// ExpiryDate es nil cuando no se cobró primer pago — el membership
	// queda en pending_payment hasta el primer abono (parcial o total).
	ExpiryDate          *time.Time
	MembershipStatus    string // "active" | "pending_payment"
	PendingFirstPayment bool
	MemberNumber        int
	Dispatch            WelcomeDispatchResult
	PaymentID           *uuid.UUID
	PaymentFolio        string
	PaymentTotal        float64
	// Datos de la promo aplicada al primer pago — vacíos cuando no hubo.
	PromotionAppliedID *uuid.UUID
	PromotionName      string
	PromotionKind      string
	PromotionExtraDays int
	PromotionGiftedIDs []uuid.UUID
}

type CreateMember struct {
	Members         memRepo.MemberRepository
	Memberships     memRepo.MembershipRepository
	MembershipTypes memRepo.MembershipTypeRepository
	// Payments + Folios sólo se usan cuando ChargeFirstPayment=true. En
	// tests fixtures que sólo crean socios sin cobro pueden pasar nil.
	Payments billingRepo.PaymentRepository
	Folios   *folioSvc.Generator
	// Welcome es la seam cross-BC que la notifications BC implementa
	// (ver notifications/app/EnqueueWelcomePin). Nil = no se envía
	// notificación; usado en tests que no exercisan WhatsApp.
	Welcome WelcomeNotifier
	// Receipt es la seam del RECIBO del primer pago. CreateMember cobra el
	// primer pago directo (sin pasar por el EventPublisher de billing), así
	// que sin esto el recibo del alta nunca se encola (sí el de renovación).
	// Se invoca POST-commit. Nil = no se envía.
	Receipt PaymentReceiptNotifier
	// Digits resuelve/crece la longitud del número de socio por gym
	// (ADR-010). Nil = default fijo (4 dígitos, sin bump).
	Digits MemberNumberDigitsStore
	// Promotions (opcional) habilita aplicar una promo al primer pago.
	// Nil = ignora cualquier Promotion del input.
	Promotions *promoApp.ApplyPromotion
	// MemberSvc (opcional) habilita los efectos secundarios de promos
	// (extra_days adjustment, companion memberships) en la misma tx del
	// alta. Nil = sólo se honran promos sin efectos (percent, fixed,
	// free_enrollment).
	MemberSvc *MemberService
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
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

// WithWelcomeNotifier wires the WhatsApp welcome seam. Idempotent chaining
// helper for DI in cmd/server + cmd/sidecar; tests can skip calling it and
// the use case defaults to noopWelcomeNotifier.
func (uc *CreateMember) WithWelcomeNotifier(n WelcomeNotifier) *CreateMember {
	uc.Welcome = n
	return uc
}

// WithReceiptNotifier engancha el seam del recibo del primer pago. Sin esto,
// el alta con primer pago NO encola recibo (la renovación sí, vía
// RegisterMembershipPayment → EventPublisher). Se invoca post-commit.
func (uc *CreateMember) WithReceiptNotifier(n PaymentReceiptNotifier) *CreateMember {
	uc.Receipt = n
	return uc
}

// WithMemberNumberDigits wires the gyms-backed config seam (length + bump)
// so auto-assignment honors the per-gym member_number_digits (ADR-010).
func (uc *CreateMember) WithMemberNumberDigits(s MemberNumberDigitsStore) *CreateMember {
	uc.Digits = s
	return uc
}

// WithPromotions engancha el use case de promos para que el primer pago
// del alta pueda aplicar una. Sin este setter el campo Promotion del
// input se ignora silenciosamente (caso tests / builds sin BC promotions).
func (uc *CreateMember) WithPromotions(p *promoApp.ApplyPromotion, svc *MemberService) *CreateMember {
	uc.Promotions = p
	uc.MemberSvc = svc
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
	// Datos del recibo del primer pago — se capturan DENTRO de la tx (donde
	// viven p/mt/ms) y se encolan POST-commit. nil = no hubo primer pago.
	var receiptIn *PaymentReceiptInput
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

		// 4.5) Auto-asignar número de socio (ADR-010). Históricamente
		//      "Asignar PIN" era un paso manual post-inscripción; ahora
		//      siempre arranca con un número generado (público, visible en
		//      el perfil para que el operador lo lea y lo escriba en la
		//      credencial). Si la generación llegara a agotar el espacio
		//      (irreal con el bump-al-50%) loguueamos y seguimos: el socio
		//      queda sin número y se le asigna después manualmente — eso es
		//      mejor que abortar toda la inscripción.
		digits := uc.Digits
		if digits == nil {
			digits = defaultMemberNumberDigitsStore{}
		}
		generated, gerr := generateUniqueMemberNumber(tx, uc.Members, digits, in.GymID)
		if gerr == nil {
			m.SetMemberNumber(generated, now)
			out.MemberNumber = generated
		} else {
			log.Printf("[create-member] no se pudo auto-asignar número de socio (gym=%s member=%s): %v", in.GymID, m.ID, gerr)
		}

		if _, err := uc.Members.Create(tx, m); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 4.6) Encolar la notificación de WhatsApp con el número de socio,
		//      PERO sólo si el socio queda ACTIVO en esta alta (hay primer
		//      pago). Si se crea en pending_payment, la bienvenida se difiere
		//      a su primer pago (RegisterMembershipPayment la encola cuando la
		//      membresía se activa por primera vez). "Quedar activo" = ya pagó.
		//      La seam decide solita si saltarse (gym sin WhatsApp, sin
		//      teléfono, toggle off) — devuelve Dispatched=false con
		//      SkippedReason estable. Va en la misma tx para que el row de
		//      notification_queue no quede huérfano si después algo falla.
		if out.MemberNumber != 0 && in.ChargeFirstPayment {
			notifier := uc.Welcome
			if notifier == nil {
				notifier = noopWelcomeNotifier{}
			}
			res, nerr := notifier.Notify(ctx, tx, WelcomeNotifyInput{
				GymID: in.GymID, MemberID: m.ID, Number: out.MemberNumber,
			}, now)
			if nerr != nil {
				return nerr
			}
			out.Dispatch = res
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
				"folio":                  m.Folio,
				"membership_type_id":     mt.ID,
				"membership_id":          ms.ID,
				"membership_status":      ms.Status,
				"expiry_date":            formatExpiryForAudit(ms.ExpiryDate),
				"first_payment":          in.ChargeFirstPayment,
				"member_number_assigned": out.MemberNumber != 0,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})

		// Preserve auto-asignado número + dispatch result (set above in
		// steps 4.5 / 4.6) — el reset de `out` mantiene el resto.
		generatedNumber := out.MemberNumber
		dispatch := out.Dispatch
		out = CreateMemberOutput{
			MemberID:            m.ID,
			MembershipID:        ms.ID,
			Folio:               m.Folio,
			ExpiryDate:          ms.ExpiryDate,
			MembershipStatus:    ms.Status,
			PendingFirstPayment: in.ChargeFirstPayment,
			MemberNumber:        generatedNumber,
			Dispatch:            dispatch,
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

			// 6.1) Promo opcional. Resolve antes de crear el payment para
			//      tener el discount listo + el effect a persistir post-create.
			//      Validación + límites + targeting viven en ApplyPromotion.
			paymentID := uuid.New()
			var promoResult *promoApp.ApplyPromotionResult
			var discount float64
			var discountReason *string
			if in.Promotion != nil && uc.Promotions != nil {
				memID := m.ID
				res, perr := uc.Promotions.Resolve(ctx, tx, promoApp.ApplyPromotionInput{
					GymID:              in.GymID,
					ActorUserID:        in.ActorUserID,
					PromotionID:        in.Promotion.PromotionID,
					Code:               in.Promotion.Code,
					PaymentID:          paymentID,
					Target:             promoDomain.AppliesToMembership,
					Subtotal:           subtotal,
					EnrollmentFee:      enrollmentAmt,
					HasEnrollment:      chargeEnrollment,
					MemberID:           &memID,
					CompanionMemberIDs: in.Promotion.CompanionMemberIDs,
					Notes:              in.Promotion.Notes,
					Today:              in.StartDate,
				}, now)
				if perr != nil {
					return perr
				}
				promoResult = res
				if res.Discount > 0 {
					discount = res.Discount
					reason := res.DiscountReason
					discountReason = &reason
				}
			}

			folioPay, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptMembership)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			paid := subtotal - discount
			p, err := paymentDomain.NewMembershipPayment(
				paymentID, in.GymID, in.ActorUserID, m.ID, folioPay,
				subtotal, discount, paid, 0,
				in.PaymentMethod, in.StartDate, now, nil, discountReason,
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

			// 6.2) Promo: persistir applied_promotion + efectos. Hacemos
			//      esto DESPUÉS de Payments.Create para que el FK al
			//      payment_id se satisfaga.
			giftedIDs := []uuid.UUID(nil)
			if promoResult != nil {
				if err := uc.Promotions.Persist(ctx, tx, promoResult, p.ID, now); err != nil {
					return err
				}
				if promoResult.ExtraDays > 0 && uc.MemberSvc != nil {
					if err := uc.MemberSvc.ApplyMembershipAdjustment(ctx, tx, ApplyMembershipAdjustmentInput{
						GymID:        in.GymID,
						MembershipID: ms.ID,
						Days:         promoResult.ExtraDays,
						Reason:       "promo: " + promoResult.Promotion.Name,
						ActorUserID:  in.ActorUserID,
					}, now); err != nil {
						return err
					}
				}
				if len(promoResult.CompanionMemberIDs) > 0 && uc.MemberSvc != nil {
					for _, companionID := range promoResult.CompanionMemberIDs {
						gifted, gerr := uc.MemberSvc.GiftMembership(ctx, tx, GiftMembershipInput{
							GymID:             in.GymID,
							CompanionMemberID: companionID,
							MembershipTypeID:  mt.ID,
							StartDate:         in.StartDate,
						}, now)
						if gerr != nil {
							return gerr
						}
						giftedIDs = append(giftedIDs, gifted.ID)
					}
				}
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
			// Capturar datos del recibo del primer pago — se encola tras el
			// commit (paso post-tx). Plan + vigencia REALES, igual que el
			// recibo de renovación (RegisterMembershipPayment).
			receiptIn = &PaymentReceiptInput{
				GymID:              in.GymID,
				PaymentID:          paymentID,
				MemberID:           m.ID,
				Concept:            p.Concept,
				Amount:             p.Amount,
				Folio:              p.Folio,
				MembershipTypeName: mt.Name,
				NewExpiry:          ms.ExpiryDate,
			}
			if promoResult != nil {
				appliedID := promoResult.AppliedID
				out.PromotionAppliedID = &appliedID
				out.PromotionName = promoResult.Promotion.Name
				out.PromotionKind = promoResult.Promotion.Kind
				if promoResult.ExtraDays > 0 {
					out.PromotionExtraDays = promoResult.ExtraDays
					// El expiry reportado debe reflejar el ajuste.
					if ms.ExpiryDate != nil {
						adjusted := ms.ExpiryDate.AddDate(0, 0, promoResult.ExtraDays)
						out.ExpiryDate = &adjusted
					}
				}
				out.PromotionGiftedIDs = giftedIDs
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Recibo del primer pago: se encola POST-commit (EnqueueReceipt abre su
	// propia tx y no debe anidarse en la del alta). Best-effort — si falla, el
	// pago ya commiteó y el dispatcher reintenta. El welcome, en cambio, va
	// dentro de la tx porque comparte el row de notification_queue del alta.
	if receiptIn != nil {
		notifier := uc.Receipt
		if notifier == nil {
			notifier = noopPaymentReceiptNotifier{}
		}
		notifier.NotifyPaymentReceipt(ctx, *receiptIn)
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
	// Canónico (src/shared/phone) — el MISMO que usa el dominio al guardar, así
	// el valor que se valida y con el que se hace el dedup (ExistsByGymAndPhone)
	// es IDÉNTICO al que termina en BD (E.164 con +52). Antes el dedup usaba un
	// trimSpaces sin código de país y no macheaba el valor guardado.
	v := phonepkg.Normalize(raw)
	if !memberDomain.ValidatePhone(v) {
		return "", memErrors.ErrInvalidPhone
	}
	return v, nil
}
