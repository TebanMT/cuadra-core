package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CashSessionOpenChecker tells whether the cash session that covers a recorded
// movement is still open, i.e. nobody has counted it yet. It runs in the
// caller's transaction.
type CashSessionOpenChecker interface {
	CoveredByOpenCashSession(context.Context, shared.Transaction, CashSessionAdjustmentInput) (bool, error)
}

type VoidOwnCashMovementInput struct {
	GymID, ActorUserID, MovementID uuid.UUID
	ExpectedVersion                int
	Reason                         string
}

// VoidOwnCashMovement lets the person at the front desk take back their own
// capture (a $500 outflow typed instead of $50) before the drawer is counted,
// so the count is not signed with someone else's mistake. Once the session is
// closed only the owner corrects it, through the regular correction flows.
type VoidOwnCashMovement struct {
	Movements expRepo.CashMovementRepository
	Expenses  expRepo.ExpenseRepository
	Sessions  CashSessionOpenChecker
	UoW       shared.UnitOfWork
	Audit     audit.Recorder
}

func NewVoidOwnCashMovement(movements expRepo.CashMovementRepository, expenses expRepo.ExpenseRepository,
	sessions CashSessionOpenChecker, uow shared.UnitOfWork, recorder audit.Recorder) *VoidOwnCashMovement {
	return &VoidOwnCashMovement{Movements: movements, Expenses: expenses, Sessions: sessions, UoW: uow, Audit: recorder}
}

func (uc *VoidOwnCashMovement) Execute(ctx context.Context, in VoidOwnCashMovementInput) error {
	reason, err := normalizedCorrectionReason(in.Reason)
	if err != nil {
		return err
	}
	if uc.Sessions == nil {
		return shared.NewUnexpectedError(errors.New("cash session checker is not configured"))
	}
	now := time.Now().UTC()
	return uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		m, err := uc.Movements.GetByID(tx, in.GymID, in.MovementID)
		if err != nil {
			return err
		}
		if in.ExpectedVersion > 0 && in.ExpectedVersion != m.Version {
			return shared.NewBusinessError(expErrors.ErrVersionConflict, "")
		}
		if m.ClassificationStatus == cashDomain.AsInventoryPurchase {
			return shared.NewBusinessError(expErrors.ErrLinkedCashMovement, "")
		}
		if !m.CanBeVoidedBy(in.ActorUserID) {
			return shared.NewBusinessError(expErrors.ErrVoidNotOwnMovement, "")
		}
		drawerID := m.CashDrawerID
		if drawerID == uuid.Nil {
			drawerID = in.GymID
		}
		open, err := uc.Sessions.CoveredByOpenCashSession(ctx, tx, CashSessionAdjustmentInput{
			GymID: in.GymID, ActorUserID: in.ActorUserID, OperationalDate: m.MovementOn,
			OriginalRecordedAt: m.CreatedAt, DrawerID: drawerID, Reason: reason,
		})
		if err != nil {
			return err
		}
		if !open {
			return shared.NewBusinessError(expErrors.ErrVoidCashSessionNotOpen, "")
		}

		// "Pagar un gasto" from the drawer created the expense together with
		// the outflow; taking the outflow back takes the expense with it.
		var voidedExpenseID *uuid.UUID
		if m.ClassificationStatus == cashDomain.AsExpense {
			if m.ExpenseID == nil || uc.Expenses == nil {
				return shared.NewBusinessError(expErrors.ErrLinkedCashMovement, "")
			}
			e, expenseErr := uc.Expenses.GetByID(tx, in.GymID, *m.ExpenseID)
			if expenseErr != nil {
				return expenseErr
			}
			if e.Source != expenseDomain.SourceCashMovement || e.CashMovementID == nil ||
				*e.CashMovementID != m.ID || e.RecurringOccurrenceID != nil {
				return shared.NewBusinessError(expErrors.ErrLinkedExpense, "")
			}
			e.SoftDelete(now)
			if _, expenseErr = uc.Expenses.Update(tx, e); expenseErr != nil {
				return shared.NewUnexpectedError(expenseErr)
			}
			if expenseErr = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expenses", e.ID,
				"void_with_cash_movement", map[string]any{
					"cash_movement_id": m.ID, "amount": e.Amount, "category": e.Category,
					"correction_reason": reason,
				}, now); expenseErr != nil {
				return expenseErr
			}
			id := e.ID
			voidedExpenseID = &id
		} else if m.ExpenseID != nil {
			return shared.NewBusinessError(expErrors.ErrLinkedCashMovement, "")
		}

		version := m.Version
		m.SoftDelete(now)
		if _, err = uc.Movements.Update(tx, m, version); err != nil {
			return shared.NewUnexpectedError(err)
		}
		return recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", m.ID,
			"void_own", map[string]any{
				"amount": m.Amount, "movement_type": m.MovementType, "reason": m.Reason,
				"classification_status": m.ClassificationStatus, "cash_drawer_id": drawerID,
				"voided_expense_id": voidedExpenseID, "correction_reason": reason,
			}, now)
	})
}
