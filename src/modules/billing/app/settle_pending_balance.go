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
	CashDrawerID    *uuid.UUID
	PaymentDate     time.Time
	Notes           *string
	IdempotencyKey  string
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
	// Gyms (opcional) → default de PaymentDate en el día LOCAL del gym
	// (ver gymLocalPaymentDate). Nil = día UTC (tests viejos).
	Gyms        gymRepo.GymRepository
	CashDrawers CashDrawerValidator
}

func (uc *SettlePendingBalance) WithCashDrawers(v CashDrawerValidator) *SettlePendingBalance {
	uc.CashDrawers = v
	return uc
}

// WithGyms cablea el repo de gyms para anclar el default de PaymentDate
// al día calendario del gym en SU zona horaria.
func (uc *SettlePendingBalance) WithGyms(g gymRepo.GymRepository) *SettlePendingBalance {
	uc.Gyms = g
	return uc
}

func NewSettlePendingBalance(payments billingRepo.PaymentRepository, folios *folioSvc.Generator,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *SettlePendingBalance {
	return &SettlePendingBalance{Payments: payments, Folios: folios, UoW: uow, Audit: recorder}
}

func (uc *SettlePendingBalance) Execute(ctx context.Context, in SettlePendingBalanceInput) (*SettlePendingBalanceOutput, error) {
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
	var out SettlePendingBalanceOutput
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		paymentDay, dateErr := resolveMonetaryDate(tx, uc.Gyms, in.GymID, in.PaymentDate, now)
		if dateErr != nil {
			return dateErr
		}
		in.PaymentDate = paymentDay
		if replayed, err := replayPaymentCommand(tx, uc.Payments, in.GymID, key, fingerprint,
			paymentDomain.ConceptBalanceSettlement, &out); err != nil {
			return err
		} else if replayed {
			return nil
		}
		if err := validateRequestedCashDrawer(uc.CashDrawers, tx, in.GymID, in.Method, in.CashDrawerID); err != nil {
			return err
		}
		parent, err := uc.Payments.GetByID(tx, in.ParentPaymentID)
		if err != nil {
			return err
		}
		if parent.GymID != in.GymID {
			return sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
		}
		if err := validateMonetaryChronology(in.PaymentDate, parent.PaymentDate); err != nil {
			return err
		}

		folio, err := uc.Folios.Next(tx, in.GymID, paymentDomain.ConceptBalanceSettlement)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// Construimos el settlement primero (validación contra parent
		// PRE-decremento) pero NO lo persistimos todavía — necesitamos
		// que el upsert del parent aterrice antes en sync_queue. El
		// proyector del cloud aplica items por rowid; si por alguna
		// razón el parent no existe en postgres (sync fallido previo,
		// orden inverso del batch tras una recuperación parcial, etc.),
		// el upsert del parent lo crea ahora y el INSERT del settlement
		// satisface el FK parent_payment_id.
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
		if in.CashDrawerID != nil {
			settlement.WithCashDrawer(*in.CashDrawerID)
		}
		if err := beginPaymentCommand(settlement, key, fingerprint); err != nil {
			return err
		}

		// 1) Decremento + persistencia del parent (enqueue UPSERT a rowid M).
		newBalance, err := parent.DecrementBalance(in.Amount, now)
		if err != nil {
			return sharedDomain.NewBusinessError(err, "")
		}
		if _, err := uc.Payments.Update(tx, parent); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// 2) Persistencia del settlement (enqueue INSERT a rowid M+1).
		if _, err := uc.Payments.Create(tx, settlement); err != nil {
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
		if err := finalizePaymentCommand(tx, uc.Payments, settlement, out, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
