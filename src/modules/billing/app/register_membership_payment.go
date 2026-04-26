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
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RegisterMembershipPaymentInput carries everything UC-018 needs. The use case
// resolves the MembershipType inside its tx and decides whether to charge the
// enrollment / maintenance fees automatically. `Discount` and the partial
// payment fields are optional — pass zero values to skip.
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

	// Partial payment: when PaidNow > 0 and < total, BalancePending = total - paid.
	// When PaidNow == 0 the use case treats it as full payment (paid = total).
	PaidNow float64
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
}

type RegisterMembershipPayment struct {
	Payments  billingRepo.PaymentRepository
	Folios    *folioSvc.Generator
	MemberSvc *memApp.MemberService
	Members   memRepo.MemberRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
	Publisher EventPublisher
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

func (uc *RegisterMembershipPayment) Execute(ctx context.Context, in RegisterMembershipPaymentInput) (*RegisterMembershipPaymentOutput, error) {
	now := time.Now().UTC()
	if in.PaymentDate.IsZero() {
		in.PaymentDate = now
	}
	if in.Method == "" {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrPaymentMethodMissing)
	}

	var (
		out RegisterMembershipPaymentOutput
		evt PaymentCompletedEvent
	)
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
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

		// 3. Compute the base amount using the (now-loaded) MembershipType.
		mt := renewed.NextType
		subtotal := mt.Price
		chargeEnrollment := !member.EnrollmentPaid && mt.EnrollmentFee > 0
		if chargeEnrollment {
			subtotal += mt.EnrollmentFee
		}
		chargeMaintenance := shouldChargeMaintenance(member, mt, in.PaymentDate)
		if chargeMaintenance {
			subtotal += mt.MaintenanceFee
		}

		// 4. Resolve paid / balance.
		discount := in.Discount
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
		paymentID := uuid.New()
		p, err := paymentDomain.NewMembershipPayment(
			paymentID, in.GymID, in.ActorUserID, in.MemberID, folio,
			subtotal, discount, paid, balancePending,
			in.Method, in.PaymentDate, now, in.Notes, in.DiscountReason,
		)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
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
			NewExpiry:       renewed.NewMembership.ExpiryDate,
			EnrollmentChrg:  chargeEnrollment,
			MaintenanceChrg: chargeMaintenance,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	uc.Publisher.PublishPaymentCompleted(ctx, evt)
	return &out, nil
}

// shouldChargeMaintenance encodes the UC-018 rule:
//
//	frequency=monthly -> always charge
//	frequency=annual  -> charge when last_maintenance is null OR ≥365 days ago
//	no frequency      -> never charge
func shouldChargeMaintenance(member *memberDomain.Member, mt *mtDomain.MembershipType, paymentDate time.Time) bool {
	if mt.MaintenanceFee <= 0 || mt.MaintenanceFrequency == nil {
		return false
	}
	switch *mt.MaintenanceFrequency {
	case mtDomain.FrequencyMonthly:
		return true
	case mtDomain.FrequencyAnnual:
		if member.LastMaintenancePaid == nil {
			return true
		}
		return paymentDate.Sub(*member.LastMaintenancePaid) >= 365*24*time.Hour
	}
	return false
}
