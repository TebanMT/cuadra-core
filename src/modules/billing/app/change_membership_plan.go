// UC-048 (ChangeMembershipPlan): cambio de plan a mitad de ciclo (upgrade o
// cambio lateral). Crea una Membership nueva con el snapshot del tipo nuevo
// CONSERVANDO la fecha de vencimiento actual (DA-48.2) y cobra la diferencia
// prorrateada dentro del mismo UnitOfWork.Command: cobro, cambio y audit
// commitean o se revierten juntos.
//
// Reglas de cobro (la regla de membresías vive en members; billing orquesta):
//
//	delta        = precio_default del tipo nuevo − snapshot de la actual
//	días_ciclo   = vencimiento − inicio de la membresía actual (fechas reales)
//	días_restantes = vencimiento − hoy (hoy cuenta: el acceso nuevo empieza hoy)
//	proporcional = round(delta × días_restantes / días_ciclo) a peso entero
//
// El monto viene del request (la UI ofrece proporcional o diferencia
// completa, DA-48.3) pero el BE valida 0 ≤ monto ≤ delta. Descuento con
// razón obligatoria y pago parcial con la misma mecánica de UC-018.
// Enrollment y mantenimiento NO se cobran aquí (DA-48.5). Monto final $0
// (cambio lateral o descuento total) NO crea Payment (DA-48.6): el rastro
// queda en audit_log (action `plan_change`) y en el histórico de memberships.
package app

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Opciones de cobro reportadas en el audit_log (DA-48.3). "lateral" cubre el
// cambio entre planes del mismo precio (delta $0, sin cobro).
const (
	planChangeOptionProrated = "proporcional"
	planChangeOptionFull     = "completa"
	planChangeOptionLateral  = "lateral"
)

// ChangeMembershipPlanInput carries everything UC-048 needs. El cambio aplica
// HOY siempre (sin backdating en v1): no hay PaymentDate en el input — el use
// case resuelve el día local del gym y prorratea contra él.
type ChangeMembershipPlanInput struct {
	GymID               uuid.UUID
	ActorUserID         uuid.UUID
	MemberID            uuid.UUID
	NewMembershipTypeID uuid.UUID

	// Amount es el monto elegido por el operador ANTES de descuento:
	// proporcional (recomendado) o diferencia completa. El BE sólo valida
	// 0 ≤ Amount ≤ delta — sin edición libre hacia arriba (DA-48.3).
	Amount float64

	// Discount > 0 requires DiscountReason (misma mecánica DA-18.2).
	Discount       float64
	DiscountReason *string

	// Partial payment: when PaidNow > 0 and < total, BalancePending = total − paid.
	// When PaidNow == 0 the use case treats it as full payment (DA-18.1).
	PaidNow float64

	// Method / CashDrawerID sólo se exigen cuando el total es > 0 (con
	// monto final $0 no se crea Payment y no hay nada que atribuir a caja).
	Method       string
	CashDrawerID *uuid.UUID
	Notes        *string

	IdempotencyKey string
}

// ChangeMembershipPlanOutput is what the controller returns. PaymentID is nil
// when the final total was $0 (DA-48.6). Expiry es la vigencia CONSERVADA —
// el toast muestra "Sigue venciendo el DD MMM".
type ChangeMembershipPlanOutput struct {
	PaymentID       *uuid.UUID
	Folio           string
	Delta           float64
	Prorated        float64
	Subtotal        float64
	Discount        float64
	Total           float64
	Paid            float64
	BalancePending  float64
	NewMembershipID uuid.UUID
	Expiry          time.Time
	OldTypeName     string
	NewTypeName     string
	ChargeOption    string
	DaysRemaining   int
	CycleDays       int
}

type ChangeMembershipPlan struct {
	Payments  billingRepo.PaymentRepository
	Folios    *folioSvc.Generator
	MemberSvc *memApp.MemberService
	Members   memRepo.MemberRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
	Publisher EventPublisher
	// Gyms (opcional en tests legacy) ancla "hoy" al día LOCAL del gym.
	Gyms        gymRepo.GymRepository
	CashDrawers CashDrawerValidator
}

func NewChangeMembershipPlan(
	payments billingRepo.PaymentRepository,
	folios *folioSvc.Generator,
	memberSvc *memApp.MemberService,
	members memRepo.MemberRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
	publisher EventPublisher,
) *ChangeMembershipPlan {
	if publisher == nil {
		publisher = NoopPublisher{}
	}
	return &ChangeMembershipPlan{
		Payments:  payments,
		Folios:    folios,
		MemberSvc: memberSvc,
		Members:   members,
		UoW:       uow,
		Audit:     recorder,
		Publisher: publisher,
	}
}

// WithGyms cablea el repo de gyms para resolver "hoy" en la zona horaria
// del gym (mismo criterio que UC-018).
func (uc *ChangeMembershipPlan) WithGyms(g gymRepo.GymRepository) *ChangeMembershipPlan {
	uc.Gyms = g
	return uc
}

func (uc *ChangeMembershipPlan) WithCashDrawers(v CashDrawerValidator) *ChangeMembershipPlan {
	uc.CashDrawers = v
	return uc
}

func (uc *ChangeMembershipPlan) Execute(ctx context.Context, in ChangeMembershipPlanInput) (*ChangeMembershipPlanOutput, error) {
	key, err := validatePaymentCommandKey(in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	in.IdempotencyKey = key
	fingerprint, err := paymentCommandFingerprint(in)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := time.Now().UTC()

	if in.Amount < 0 || in.Discount < 0 || in.PaidNow < 0 {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrAmountInvalid)
	}

	var (
		out       ChangeMembershipPlanOutput
		evt       PaymentCompletedEvent
		charged   bool
		wasReplay bool
	)
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		// Sin backdating (UC-048): el cambio aplica hoy siempre, resuelto en
		// el día local del gym. Por eso no hay PaymentDate en el input.
		today, dateErr := strictGymLocalPaymentDate(tx, uc.Gyms, in.GymID, now)
		if dateErr != nil {
			return dateErr
		}
		// Replay de retries: sólo los cambios CON cobro dejan registro (el
		// payment row guarda el resultado). Un cambio $0 no crea Payment —
		// su retry rebota con "ya tiene ese plan", que es inofensivo porque
		// no hay dinero de por medio y el cambio ya quedó aplicado.
		if replayed, err := replayPaymentCommand(tx, uc.Payments, in.GymID, key, fingerprint,
			paymentDomain.ConceptMembership, &out); err != nil {
			return err
		} else if replayed {
			wasReplay = true
			return nil
		}
		// Operator permissions (UC-007): mismos gates que UC-018 — un
		// descuento manual necesita PermDiscounts; dejar saldo, PermCredit.
		if in.Discount > 0 {
			if err := sharedDomain.RequirePermission(ctx, sharedDomain.PermDiscounts); err != nil {
				return err
			}
		}

		// 1. Member sanity check: must exist + belong to gym.
		member, err := uc.Members.GetByID(tx, in.MemberID)
		if err != nil {
			return err
		}
		if member.GymID != in.GymID {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}

		// 2. Plan change in `members`: valida activa/no vencida/upgrade-o-
		//    lateral, crea la fila nueva conservando el vencimiento y marca
		//    la actual como replaced. Rollback total si el cobro falla.
		changed, err := uc.MemberSvc.ChangeMembershipPlan(ctx, tx, memApp.ChangeMembershipPlanInput{
			MemberID:  in.MemberID,
			NewTypeID: in.NewMembershipTypeID,
			Today:     today,
		}, now)
		if err != nil {
			return err
		}
		old, next, mt := changed.OldMembership, changed.NewMembership, changed.NewType

		// 3. Prorrateo contra las fechas REALES de la membresía anterior
		//    (respeta ajustes manuales de UC-017). Hoy cuenta como restante.
		expiry := *next.ExpiryDate
		cycleDays := daysBetween(old.StartDate, expiry)
		daysRemaining := daysBetween(today, expiry)
		delta := mt.Price - old.PriceSnapshot
		prorated := planChangeProration(delta, daysRemaining, cycleDays)

		// 4. Validar el monto elegido: nunca por encima de delta (lo demás
		//    es una renovación, no un cambio). Hacia abajo sólo descuento
		//    con razón (DA-48.3).
		if toCents(in.Amount) > toCents(delta) {
			return sharedDomain.NewValidationError(billingErrors.ErrPlanChangeAmountExceedsDelta)
		}
		subtotal := in.Amount
		discount := in.Discount
		if toCents(discount) > toCents(subtotal) {
			return sharedDomain.NewValidationError(billingErrors.ErrDiscountTooLarge)
		}
		if discount > 0 && (in.DiscountReason == nil || strings.TrimSpace(*in.DiscountReason) == "") {
			return sharedDomain.NewValidationError(billingErrors.ErrDiscountReasonRequired)
		}
		total := subtotal - discount
		if toCents(in.PaidNow) > toCents(total) {
			return sharedDomain.NewValidationError(billingErrors.ErrPartialAmountInvalid)
		}

		option := planChangeOptionProrated
		switch {
		case toCents(delta) == 0:
			option = planChangeOptionLateral
		case toCents(in.Amount) == toCents(delta):
			option = planChangeOptionFull
		}

		out = ChangeMembershipPlanOutput{
			Delta:           delta,
			Prorated:        prorated,
			Subtotal:        subtotal,
			Discount:        discount,
			Total:           total,
			NewMembershipID: next.ID,
			Expiry:          expiry,
			OldTypeName:     old.TypeNameSnapshot,
			NewTypeName:     mt.Name,
			ChargeOption:    option,
			DaysRemaining:   daysRemaining,
			CycleDays:       cycleDays,
		}

		// 5. Cobro — sólo cuando el total es > 0 (DA-48.6: $0 no crea
		//    Payment, nada entra a caja ni a ingresos).
		var p *paymentDomain.Payment
		if toCents(total) > 0 {
			if in.Method == "" {
				return sharedDomain.NewValidationError(billingErrors.ErrPaymentMethodMissing)
			}
			if err := validateRequestedCashDrawer(uc.CashDrawers, tx, in.GymID, in.Method, in.CashDrawerID); err != nil {
				return err
			}
			paid := in.PaidNow
			if paid <= 0 {
				paid = total
			}
			var balancePending float64
			if paid < total {
				balancePending = total - paid
			}
			// A partial payment leaves a balance (fiado): PermCredit.
			if err := requireFullPayment(ctx, paid, total); err != nil {
				return err
			}
			folio, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptMembership)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			p, err = paymentDomain.NewMembershipPayment(
				uuid.New(), in.GymID, in.ActorUserID, in.MemberID, folio,
				subtotal, discount, paid, balancePending,
				in.Method, today, now, in.Notes, in.DiscountReason,
			)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if in.CashDrawerID != nil {
				p.WithCashDrawer(*in.CashDrawerID)
			}
			p.WithMembership(next.ID)
			if err := beginPaymentCommand(p, key, fingerprint); err != nil {
				return err
			}
			p.SetBreakdown([]paymentDomain.BreakdownLine{
				{Label: "Cambio de plan — " + mt.Name, Amount: subtotal},
			})
			if _, err := uc.Payments.Create(tx, p); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			pid := p.ID
			out.PaymentID = &pid
			out.Folio = p.Folio
			out.Paid = paid
			out.BalancePending = balancePending
			charged = true
		}

		// 6. Audit (DA-48.6: para el cambio $0 este rastro + el histórico
		//    de memberships son el único registro).
		auditChanges := map[string]any{
			"before": map[string]any{
				"membership_id":      old.ID,
				"membership_type_id": old.MembershipTypeID,
				"type_name":          old.TypeNameSnapshot,
				"price":              old.PriceSnapshot,
				"start_date":         old.StartDate.Format("2006-01-02"),
				"expiry_date":        expiry.Format("2006-01-02"),
			},
			"after": map[string]any{
				"membership_id":      next.ID,
				"membership_type_id": next.MembershipTypeID,
				"type_name":          next.TypeNameSnapshot,
				"price":              next.PriceSnapshot,
				"start_date":         next.StartDate.Format("2006-01-02"),
				"expiry_date":        expiry.Format("2006-01-02"),
			},
			"delta":              delta,
			"prorated_suggested": prorated,
			"amount":             subtotal,
			"discount":           discount,
			"total":              total,
			"charge_option":      option,
			"days_remaining":     daysRemaining,
			"cycle_days":         cycleDays,
		}
		if p != nil {
			auditChanges["payment_id"] = p.ID
			auditChanges["folio"] = p.Folio
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "memberships",
			EntityID:    next.ID,
			Action:      audit.ActionPlanChange,
			ActorUserID: &in.ActorUserID,
			Changes:     auditChanges,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})

		// 7. Evento (recibo WhatsApp async) + resultado idempotente — sólo
		//    cuando hubo cobro; publish ocurre tras el commit.
		if p != nil {
			mid := in.MemberID
			evt = PaymentCompletedEvent{
				GymID:              in.GymID,
				PaymentID:          p.ID,
				MemberID:           &mid,
				Concept:            p.Concept,
				Amount:             p.Amount,
				Folio:              p.Folio,
				OperatorID:         in.ActorUserID,
				BalancePending:     p.BalancePending,
				MembershipTypeName: mt.Name,
				NewExpiry:          next.ExpiryDate,
			}
			if err := finalizePaymentCommand(tx, uc.Payments, p, out, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if charged && !wasReplay {
		uc.Publisher.PublishPaymentCompleted(ctx, evt)
	}
	return &out, nil
}

// planChangeProration implements the UC-048 suggested charge:
// round(delta × daysRemaining / cycleDays) a peso entero. Clamps keep the
// suggestion inside [0, delta] on degenerate dates (ciclo de 0 días, hoy
// anterior al inicio por un ajuste manual) so it never suggests more than
// the full difference the BE itself accepts.
func planChangeProration(delta float64, daysRemaining, cycleDays int) float64 {
	if delta <= 0 {
		return 0
	}
	if daysRemaining <= 0 {
		return 0
	}
	if cycleDays <= 0 || daysRemaining >= cycleDays {
		return delta
	}
	p := math.Round(delta * float64(daysRemaining) / float64(cycleDays))
	if p > delta {
		p = delta
	}
	return p
}

// daysBetween returns whole days from a to b (dates already truncated to
// UTC midnight by the domain).
func daysBetween(a, b time.Time) int {
	return int(b.Sub(a).Hours() / 24)
}
