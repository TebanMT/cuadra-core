package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RefundPaymentInput backs UC-022. `Method` is the refund medium ("cash" /
// "transfer" / "card"). When `RevertMembership=true` and the parent payment
// is a membership cobro, the use case calls into members to undo the renewal
// (DA-22.4).
type RefundPaymentInput struct {
	GymID            uuid.UUID
	ActorUserID      uuid.UUID
	ParentPaymentID  uuid.UUID
	Reason           string
	Method           string
	Amount           float64 // optional — defaults to parent.Amount
	PaymentDate      time.Time
	RevertMembership bool
}

type RefundPaymentOutput struct {
	RefundID    uuid.UUID
	RefundFolio string
	Amount      float64
	Reverted    bool
}

type RefundPayment struct {
	Payments  billingRepo.PaymentRepository
	Folios    *folioSvc.Generator
	MemberSvc *memApp.MemberService
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
}

func NewRefundPayment(payments billingRepo.PaymentRepository, folios *folioSvc.Generator,
	memberSvc *memApp.MemberService, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *RefundPayment {
	return &RefundPayment{
		Payments: payments, Folios: folios, MemberSvc: memberSvc, UoW: uow, Audit: recorder,
	}
}

func (uc *RefundPayment) Execute(ctx context.Context, in RefundPaymentInput) (*RefundPaymentOutput, error) {
	if strings.TrimSpace(in.Reason) == "" {
		return nil, sharedDomain.NewValidationError(billingErrors.ErrRefundReasonRequired)
	}
	now := time.Now().UTC()
	if in.PaymentDate.IsZero() {
		in.PaymentDate = now
	}

	var out RefundPaymentOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		parent, err := uc.Payments.GetByID(tx, in.ParentPaymentID)
		if err != nil {
			return err
		}
		if parent.GymID != in.GymID {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}
		if !parent.IsRefundable() {
			return sharedDomain.NewBusinessError(billingErrors.ErrCannotRefundNonPayment, "")
		}
		exists, err := uc.Payments.HasRefundFor(tx, parent.ID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if exists {
			return sharedDomain.NewBusinessError(billingErrors.ErrAlreadyRefunded, "")
		}

		amount := in.Amount
		if amount <= 0 {
			amount = parent.Amount
		}

		folio, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptRefund)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		refund, err := paymentDomain.NewRefundPayment(
			uuid.New(), in.GymID, in.ActorUserID,
			parent, folio, amount, in.Method, in.Reason, in.PaymentDate, now,
		)
		if err != nil {
			if err == billingErrors.ErrCannotRefundNonPayment ||
				err == billingErrors.ErrRefundReasonRequired {
				return sharedDomain.NewBusinessError(err, "")
			}
			return sharedDomain.NewValidationError(err)
		}

		// Toque al parent para forzar un upsert que aterrice ANTES que
		// el INSERT del refund en sync_queue. Defensivo: si el parent
		// nunca llegó a postgres (sync previo fallido), su upsert lo
		// crea y el FK parent_payment_id del refund satisface.
		parent.Touch(now)
		if _, err := uc.Payments.Update(tx, parent); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		if _, err := uc.Payments.Create(tx, refund); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// Optional: revert membership renewal (DA-22.4). Only meaningful when
		// the original payment was a membership cobro and we have a member.
		reverted := false
		if in.RevertMembership && parent.Concept == paymentDomain.ConceptMembership && parent.MemberID != nil {
			if _, err := uc.MemberSvc.RevertMembershipFromPayment(ctx, tx,
				memApp.RevertMembershipFromPaymentInput{MemberID: *parent.MemberID}, now); err != nil {
				return err
			}
			reverted = true
		}

		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "payments",
			EntityID:    refund.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"folio":             refund.Folio,
				"parent_payment_id": parent.ID,
				"amount":            refund.Amount,
				"reason":            in.Reason,
				"reverted_renewal":  reverted,
				"concept":           refund.Concept,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})

		out = RefundPaymentOutput{
			RefundID:    refund.ID,
			RefundFolio: refund.Folio,
			Amount:      refund.Amount,
			Reverted:    reverted,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
