package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// SettlePendingBalanceInput backs UC-019. `Amount` may be less than the
// remaining balance (DA-19.2 — partial abonos allowed).
type SettlePendingBalanceInput struct {
	GymID           uuid.UUID
	ActorUserID     uuid.UUID
	ParentPaymentID uuid.UUID
	Amount          float64
	Method          string
	PaymentDate     time.Time
	Notes           *string
}

type SettlePendingBalanceOutput struct {
	SettlementID      uuid.UUID
	SettlementFolio   string
	NewBalancePending float64
}

type SettlePendingBalance struct {
	Payments billingRepo.PaymentRepository
	Folios   *folioSvc.Generator
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
}

func NewSettlePendingBalance(payments billingRepo.PaymentRepository, folios *folioSvc.Generator,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *SettlePendingBalance {
	return &SettlePendingBalance{Payments: payments, Folios: folios, UoW: uow, Audit: recorder}
}

func (uc *SettlePendingBalance) Execute(ctx context.Context, in SettlePendingBalanceInput) (*SettlePendingBalanceOutput, error) {
	now := time.Now().UTC()
	if in.PaymentDate.IsZero() {
		in.PaymentDate = now
	}
	var out SettlePendingBalanceOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		parent, err := uc.Payments.GetByID(tx, in.ParentPaymentID)
		if err != nil {
			return err
		}
		if parent.GymID != in.GymID {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}

		folio, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptBalanceSettlement)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		settlement, err := paymentDomain.NewBalanceSettlementPayment(
			uuid.New(), in.GymID, in.ActorUserID,
			parent, folio, in.Amount, in.Method, in.PaymentDate, now, in.Notes,
		)
		if err != nil {
			if err == billingErrors.ErrSettlementExceedsBalance ||
				err == billingErrors.ErrCannotSettleConcept ||
				err == billingErrors.ErrNoBalancePending {
				return sharedDomain.NewBusinessError(err, "")
			}
			return sharedDomain.NewValidationError(err)
		}

		if _, err := uc.Payments.Create(tx, settlement); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		newBalance, err := parent.DecrementBalance(in.Amount, now)
		if err != nil {
			return sharedDomain.NewBusinessError(err, "")
		}
		if _, err := uc.Payments.Update(tx, parent); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "payments",
			EntityID:    settlement.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"folio":             settlement.Folio,
				"parent_payment_id": parent.ID,
				"amount":            settlement.Amount,
				"new_balance":       newBalance,
				"concept":           settlement.Concept,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})

		out = SettlePendingBalanceOutput{
			SettlementID:      settlement.ID,
			SettlementFolio:   settlement.Folio,
			NewBalancePending: newBalance,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
