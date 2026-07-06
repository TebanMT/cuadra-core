// Package app holds the billing use cases. Each use case is a struct with a
// constructor + an Execute method, following the Kash DDD style.
//
// UC-018 (RegisterMembershipPayment) is the central case: it computes the
// base amount from a MembershipType (price + enrollment_fee + maintenance_fee
// when applicable), applies an optional discount, mints a folio, persists the
// Payment row, and orchestrates the consequences in `members` — renew the
// active Membership, mark enrollment paid, update last maintenance — inside
// the same UnitOfWork.Command transaction so everything commits or nothing
// does (atomicity per CUADRA-SPEC §6.4 and DA-18.x).
package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	promoApp "github.com/cuadra/cuadra-core/src/modules/promotions/app"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// PromotionApply es el sub-input opcional para aplicar una promo al
// cobro. PromotionID y Code son excluyentes (operador eligió de la
// lista vigente o escribió un código). CompanionMemberIDs sólo para
// kind=companion_memberships.
type PromotionApply struct {
	PromotionID        *uuid.UUID
	Code               *string
	CompanionMemberIDs []uuid.UUID
	Notes              *string
}

// RegisterMembershipPaymentInput carries everything UC-018 needs. The use case
// resolves the MembershipType inside its tx and decides whether to charge the
// enrollment / maintenance fees — by default automáticamente (basado en
// member.EnrollmentPaid y frequency-aware last_maintenance), pero el operador
// puede sobreescribir via ChargeEnrollment / ChargeMaintenance.
type RegisterMembershipPaymentInput struct {
	GymID            uuid.UUID
	ActorUserID      uuid.UUID
	MemberID         uuid.UUID
	MembershipTypeID uuid.UUID
	Method           string
	PaymentDate      time.Time
	Notes            *string

	// Discount > 0 requires DiscountReason.
	Discount       float64
	DiscountReason *string

	// ChargeEnrollment / ChargeMaintenance permiten al operador forzar
	// (true) o saltar (false) el cobro de cada cuota extra. Nil deja
	// que el use case decida automáticamente:
	//   * Enrollment: cobrar si !member.EnrollmentPaid && fee>0.
	//   * Maintenance: cobrar según shouldChargeMaintenance (frecuencia
	//     + last_maintenance_paid).
	// Cuando el operador pasa true para una cuota que el plan no
	// define (fee=0), se ignora silenciosamente — no inventamos cobros.
	ChargeEnrollment  *bool
	ChargeMaintenance *bool

	// EnrollmentAmount / MaintenanceAmount sobreescriben el snapshot del
	// plan cuando son > 0 (caso: el plan nació con fee=0 antes de que el
	// gym prendiera el toggle a nivel ChargeSettings; el FE resuelve el
	// monto efectivo desde gym.charge_settings y lo pasa explícitamente).
	EnrollmentAmount  float64
	MaintenanceAmount float64

	// Partial payment: when PaidNow > 0 and < total, BalancePending = total - paid.
	// When PaidNow == 0 the use case treats it as full payment (paid = total).
	PaidNow float64

	// Promotion (opcional) — cuando viene, se valida y aplica DENTRO de la
	// misma tx del cobro. MAX 1 promo por cobro (sin stacking en MVP).
	// Si la validación falla (expirada, sobre-límite, etc.) el cobro entero
	// se rollbackea.
	Promotion *PromotionApply
}

// RegisterMembershipPaymentOutput is what the controller returns. NewExpiry
// is the renewed Membership's expiry_date (so the toast can show "Vence DD MM").
type RegisterMembershipPaymentOutput struct {
	PaymentID       uuid.UUID
	Folio           string
	Subtotal        float64
	Discount        float64
	Total           float64
	Paid            float64
	BalancePending  float64
	NewMembershipID uuid.UUID
	NewExpiry       time.Time
	EnrollmentChrg  bool
	MaintenanceChrg bool
	// Datos del efecto de la promo aplicada (vacíos cuando no hubo
	// promo). El controller los expone al FE para que el comprobante
	// y el toast post-cobro muestren "Aplicamos: <Promo> — $50 off /
	// +7 días / 2x1".
	PromotionAppliedID *uuid.UUID
	PromotionName      string
	PromotionKind      string
	PromotionExtraDays int
	PromotionGiftedIDs []uuid.UUID
}

type RegisterMembershipPayment struct {
	Payments  billingRepo.PaymentRepository
	Folios    *folioSvc.Generator
	MemberSvc *memApp.MemberService
	Members   memRepo.MemberRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
	Publisher EventPublisher
	// Promotions es opcional. Si nil, el use case ignora cualquier
	// Promotion del input (caso: build legacy sin el BC enganchado).
	Promotions *promoApp.ApplyPromotion
	// Welcome (opcional) encola el WhatsApp de bienvenida cuando este pago
	// activa al socio por PRIMERA vez (membresía pasa de pending/inexistente
	// a active). Nil = no se manda (tests/builds sin notifications).
	Welcome memApp.WelcomeNotifier
	// Gyms (opcional) → default de PaymentDate en el día LOCAL del gym
	// cuando el caller no manda fecha. Nil = día UTC (tests viejos).
	Gyms gymRepo.GymRepository
}

func NewRegisterMembershipPayment(
	payments billingRepo.PaymentRepository,
	folios *folioSvc.Generator,
	memberSvc *memApp.MemberService,
	members memRepo.MemberRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
	publisher EventPublisher,
) *RegisterMembershipPayment {
	if publisher == nil {
		publisher = NoopPublisher{}
	}
	return &RegisterMembershipPayment{
		Payments:  payments,
		Folios:    folios,
		MemberSvc: memberSvc,
		Members:   members,
		UoW:       uow,
		Audit:     recorder,
		Publisher: publisher,
	}
}

// WithPromotions engancha el use case de promociones para que cobros
// con `Promotion != nil` se validen y apliquen dentro de la misma tx.
func (uc *RegisterMembershipPayment) WithPromotions(p *promoApp.ApplyPromotion) *RegisterMembershipPayment {
	uc.Promotions = p
	return uc
}

// WithWelcomeNotifier engancha el seam de bienvenida (el mismo que usa
// CreateMember). El welcome se dispara sólo en la PRIMERA activación del
// socio (su primer pago activa la membresía), no en renovaciones.
func (uc *RegisterMembershipPayment) WithWelcomeNotifier(n memApp.WelcomeNotifier) *RegisterMembershipPayment {
	uc.Welcome = n
	return uc
}

// WithGyms cablea el repo de gyms para anclar el default de PaymentDate
// al día calendario del gym en SU zona horaria (ver gymLocalPaymentDate).
func (uc *RegisterMembershipPayment) WithGyms(g gymRepo.GymRepository) *RegisterMembershipPayment {
	uc.Gyms = g
	return uc
}

func (uc *RegisterMembershipPayment) Execute(ctx context.Context, in RegisterMembershipPaymentInput) (*RegisterMembershipPaymentOutput, error) {
	now := time.Now().UTC()
	if in.Method == "" {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentMethodMissing)
	}

	var (
		out RegisterMembershipPaymentOutput
		evt PaymentCompletedEvent
	)
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		// 0. Default de fecha: el día LOCAL del gym, no el día UTC. Un
		// cobro a las 10 PM de CDMX pertenece al día en curso — anclarlo
		// en UTC corría el vencimiento de la renovación +1 día y mandaba
		// el pago a la caja del día siguiente.
		if in.PaymentDate.IsZero() {
			in.PaymentDate = gymLocalPaymentDate(tx, uc.Gyms, in.GymID, now)
		}

		// 1. Member sanity check: must exist + belong to gym.
		member, err := uc.Members.GetByID(tx, in.MemberID)
		if err != nil {
			return err
		}
		if member.GymID != in.GymID {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}

		// 2. Renew the membership in `members` (it fetches the type internally,
		//    validates active, snapshots into the new Membership row).
		renewed, err := uc.MemberSvc.RenewMembershipForPayment(ctx, tx, memApp.RenewMembershipForPaymentInput{
			MemberID:         in.MemberID,
			MembershipTypeID: in.MembershipTypeID,
			PaymentDate:      in.PaymentDate,
		}, now)
		if err != nil {
			return err
		}

		// 2.5 Welcome de WhatsApp — sólo cuando este pago ACTIVA al socio por
		// primera vez. renewed.OldMembership==nil marca esa transición (Caso A
		// pending→active o Caso 0 re-inscripción de huérfano), no una renovación
		// (Caso B deja OldMembership != nil). Idempotente por (socio, número) en
		// el seam, así que renovar nunca re-envía. Requiere número asignado.
		if uc.Welcome != nil && renewed.OldMembership == nil &&
			member.MemberNumber != nil && *member.MemberNumber != 0 {
			if _, werr := uc.Welcome.Notify(ctx, tx, memApp.WelcomeNotifyInput{
				GymID: in.GymID, MemberID: in.MemberID, Number: *member.MemberNumber,
			}, now); werr != nil {
				return werr
			}
		}

		// 3. Compute the base amount using the (now-loaded) MembershipType.
		//    Las cuotas extra siguen estas reglas:
		//      * Monto efectivo = override (>0) ?? snapshot del plan.
		//      * Cobrar = override del operador (no-nil) ?? auto-decisión.
		//      * Si el monto efectivo es 0, NO se cobra aunque el flag
		//        diga true — no inventamos cobros.
		mt := renewed.NextType

		enrollmentAmt := mt.EnrollmentFee
		if in.EnrollmentAmount > 0 {
			enrollmentAmt = in.EnrollmentAmount
		}
		autoEnrollment := !member.EnrollmentPaid && enrollmentAmt > 0
		chargeEnrollment := autoEnrollment
		if in.ChargeEnrollment != nil {
			chargeEnrollment = *in.ChargeEnrollment && enrollmentAmt > 0
		}

		maintenanceAmt := mt.MaintenanceFee
		if in.MaintenanceAmount > 0 {
			maintenanceAmt = in.MaintenanceAmount
		}
		autoMaintenance := shouldChargeMaintenance(member, mt, in.PaymentDate) && maintenanceAmt > 0
		chargeMaintenance := autoMaintenance
		if in.ChargeMaintenance != nil {
			chargeMaintenance = *in.ChargeMaintenance && maintenanceAmt > 0
		}

		subtotal := mt.Price
		if chargeEnrollment {
			subtotal += enrollmentAmt
		}
		if chargeMaintenance {
			subtotal += maintenanceAmt
		}

		// 3.5 Promo: validar (Resolve) + dejar el effect listo para
		//     persistir después de crear el payment row. Las companion
		//     memberships y el extra_days se materializan también
		//     post-payment-create. Si la promo no valida (expirada,
		//     sobre-límite, target incompatible…), abortamos el cobro
		//     entero — el rollback de la UoW cubre el caso.
		discount := in.Discount
		discountReason := in.DiscountReason
		paymentID := uuid.New()
		var promoResult *promoApp.ApplyPromotionResult
		if in.Promotion != nil && uc.Promotions != nil {
			if discount > 0 {
				// Stacking: ni 2 promos ni promo + descuento manual.
				return sharedDomain.NewBusinessError(billingErrors.ErrDiscountTooLarge, "no puedes combinar descuento manual con promoción")
			}
			res, err := uc.Promotions.Resolve(ctx, tx, promoApp.ApplyPromotionInput{
				GymID:              in.GymID,
				ActorUserID:        in.ActorUserID,
				PromotionID:        in.Promotion.PromotionID,
				Code:               in.Promotion.Code,
				PaymentID:          paymentID,
				Target:             promoDomain.AppliesToMembership,
				Subtotal:           subtotal,
				EnrollmentFee:      enrollmentAmt,
				HasEnrollment:      chargeEnrollment,
				MemberID:           &in.MemberID,
				CompanionMemberIDs: in.Promotion.CompanionMemberIDs,
				Notes:              in.Promotion.Notes,
				Today:              in.PaymentDate,
			}, now)
			if err != nil {
				return err
			}
			promoResult = res
			if res.Discount > 0 {
				discount = res.Discount
				reason := res.DiscountReason
				discountReason = &reason
			}
		}

		// 4. Resolve paid / balance.
		total := subtotal - discount
		paid := in.PaidNow
		if paid <= 0 {
			paid = total
		}
		var balancePending float64
		if paid < total {
			balancePending = total - paid
		}

		// 5. Mint folio (FOR UPDATE inside this tx).
		folio, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptMembership)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 6. Build & persist Payment.
		p, err := paymentDomain.NewMembershipPayment(
			paymentID, in.GymID, in.ActorUserID, in.MemberID, folio,
			subtotal, discount, paid, balancePending,
			in.Method, in.PaymentDate, now, in.Notes, discountReason,
		)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		// Adjuntar el desglose por concepto para que el recibo PDF lo
		// pueda imprimir línea por línea. El plan siempre va; las cuotas
		// de inscripción / mantenimiento sólo cuando efectivamente se
		// cobraron. Usa los montos *resueltos* (override del operador o
		// snapshot del plan), no los del plan crudo.
		breakdown := []paymentDomain.BreakdownLine{
			{Label: mt.Name, Amount: mt.Price},
		}
		if chargeEnrollment {
			breakdown = append(breakdown, paymentDomain.BreakdownLine{
				Label: "Inscripción", Amount: enrollmentAmt,
			})
		}
		if chargeMaintenance {
			breakdown = append(breakdown, paymentDomain.BreakdownLine{
				Label: "Mantenimiento", Amount: maintenanceAmt,
			})
		}
		p.SetBreakdown(breakdown)
		if _, err := uc.Payments.Create(tx, p); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 7. Apply the side effects on member that are billing-driven.
		dirtyMember := false
		if chargeEnrollment {
			member.MarkEnrollmentPaid(now)
			dirtyMember = true
		}
		if chargeMaintenance {
			member.UpdateLastMaintenance(in.PaymentDate, now)
			dirtyMember = true
		}
		if dirtyMember {
			if _, err := uc.Members.Update(tx, member); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}

		// 7.5 Promo: persistir applied_promotion + ejecutar efectos
		//     (extra_days adjustment + companion memberships). Hacemos
		//     esto DESPUÉS de Payments.Create para que el FK al
		//     payment_id se satisfaga. Si algo falla, el rollback de la
		//     UoW cubre payment + applied + efectos.
		giftedIDs := []uuid.UUID(nil)
		if promoResult != nil {
			if err := uc.Promotions.Persist(ctx, tx, promoResult, p.ID, now); err != nil {
				return err
			}
			if promoResult.ExtraDays > 0 {
				if err := uc.MemberSvc.ApplyMembershipAdjustment(ctx, tx, memApp.ApplyMembershipAdjustmentInput{
					GymID:        in.GymID,
					MembershipID: renewed.NewMembership.ID,
					Days:         promoResult.ExtraDays,
					Reason:       "promo: " + promoResult.Promotion.Name,
					ActorUserID:  in.ActorUserID,
				}, now); err != nil {
					return err
				}
			}
			for _, companionID := range promoResult.CompanionMemberIDs {
				gifted, err := uc.MemberSvc.GiftMembership(ctx, tx, memApp.GiftMembershipInput{
					GymID:             in.GymID,
					CompanionMemberID: companionID,
					MembershipTypeID:  mt.ID,
					StartDate:         in.PaymentDate,
				}, now)
				if err != nil {
					return err
				}
				giftedIDs = append(giftedIDs, gifted.ID)
			}
		}

		// 8. Audit.
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "payments",
			EntityID:    p.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"folio":           p.Folio,
				"member_id":       in.MemberID,
				"membership_id":   renewed.NewMembership.ID,
				"amount":          p.Amount,
				"discount":        p.DiscountAmount,
				"balance_pending": p.BalancePending,
				"concept":         p.Concept,
				"method":          p.PaymentMethod,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})

		// 9. Capture the event data — publish happens AFTER tx commits
		//    (standard domain-event pattern; subscribers should not run in
		//    the writer's transaction).
		mid := in.MemberID
		evt = PaymentCompletedEvent{
			GymID:          in.GymID,
			PaymentID:      p.ID,
			MemberID:       &mid,
			Concept:        p.Concept,
			Amount:         p.Amount,
			Folio:          p.Folio,
			OperatorID:     in.ActorUserID,
			BalancePending: p.BalancePending,
			// Datos reales para el recibo: nombre del plan + vigencia nueva.
			MembershipTypeName: mt.Name,
			NewExpiry:          renewed.NewMembership.ExpiryDate,
		}

		out = RegisterMembershipPaymentOutput{
			PaymentID:       p.ID,
			Folio:           p.Folio,
			Subtotal:        subtotal,
			Discount:        discount,
			Total:           total,
			Paid:            paid,
			BalancePending:  balancePending,
			NewMembershipID: renewed.NewMembership.ID,
			NewExpiry:       derefTimeOrZero(renewed.NewMembership.ExpiryDate),
			EnrollmentChrg:  chargeEnrollment,
			MaintenanceChrg: chargeMaintenance,
		}
		if promoResult != nil {
			id := promoResult.AppliedID
			out.PromotionAppliedID = &id
			out.PromotionName = promoResult.Promotion.Name
			out.PromotionKind = promoResult.Promotion.Kind
			// El expiry reportado debe reflejar el ajuste de extra_days
			// si lo hubo (sin re-leer la membership desde la BD).
			if promoResult.ExtraDays > 0 {
				out.PromotionExtraDays = promoResult.ExtraDays
				out.NewExpiry = out.NewExpiry.AddDate(0, 0, promoResult.ExtraDays)
			}
			out.PromotionGiftedIDs = giftedIDs
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	uc.Publisher.PublishPaymentCompleted(ctx, evt)
	return &out, nil
}

// derefTimeOrZero unwraps *time.Time to time.Time, returning zero when nil.
// Used at the response boundary; ExpiryDate is never nil for an activated
// membership but defensive code keeps the controller code simple.
func derefTimeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// shouldChargeMaintenance encodes the UC-018 rule:
//
//	frequency=monthly    -> always charge
//	frequency=bimonthly  -> charge when last_maintenance is null OR ≥60 days ago
//	frequency=quarterly  -> charge when last_maintenance is null OR ≥90 days ago
//	frequency=semiannual -> charge when last_maintenance is null OR ≥180 days ago
//	frequency=annual     -> charge when last_maintenance is null OR ≥365 days ago
//	no frequency         -> never charge
//
// "Monthly" se trata como "siempre cobrar" porque la frecuencia es la
// misma del cobro principal del plan (mensualidad); el operador no
// quiere que el sistema "salte" el cobro de mantenimiento de un mes.
// Las frecuencias largas usan ventanas en días sin month-arithmetic
// para evitar bugs de fin-de-mes ("último día de febrero").
func shouldChargeMaintenance(member *memberDomain.Member, mt *mtDomain.MembershipType, paymentDate time.Time) bool {
	if mt.MaintenanceFee <= 0 || mt.MaintenanceFrequency == nil {
		return false
	}
	threshold := maintenanceThresholdDays(*mt.MaintenanceFrequency)
	if threshold < 0 {
		return false
	}
	if threshold == 0 {
		return true // monthly
	}
	if member.LastMaintenancePaid == nil {
		return true
	}
	return paymentDate.Sub(*member.LastMaintenancePaid) >= time.Duration(threshold)*24*time.Hour
}

// maintenanceThresholdDays returns the minimum days between maintenance
// charges for the given frequency. 0 = always charge (monthly); -1 =
// unknown frequency, never charge.
func maintenanceThresholdDays(freq string) int {
	switch freq {
	case mtDomain.FrequencyMonthly:
		return 0
	case mtDomain.FrequencyBimonthly:
		return 60
	case mtDomain.FrequencyQuarterly:
		return 90
	case mtDomain.FrequencySemiannual:
		return 180
	case mtDomain.FrequencyAnnual:
		return 365
	}
	return -1
}
