package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type DeleteExpenseInput struct {
	GymID            uuid.UUID
	ActorUserID      uuid.UUID
	ExpenseID        uuid.UUID
	ExpectedVersion  int
	CorrectionReason string
}

type DeleteExpense struct {
	Occurrences   expRepo.OccurrenceRepository
	Expenses      expRepo.ExpenseRepository
	UoW           sharedDomain.UnitOfWork
	Audit         audit.Recorder
	CashMovements expRepo.CashMovementRepository
	Policy        OperationalPolicy
	Marker        CashSessionAdjustmentMarker
}

func (uc *DeleteExpense) WithOperational(c expRepo.CashMovementRepository, p OperationalPolicy) *DeleteExpense {
	uc.CashMovements = c
	uc.Policy = p
	return uc
}

func (uc *DeleteExpense) WithCashSessionMarker(marker CashSessionAdjustmentMarker) *DeleteExpense {
	uc.Marker = marker
	return uc
}

func NewDeleteExpense(expenses expRepo.ExpenseRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *DeleteExpense {
	return &DeleteExpense{Expenses: expenses, UoW: uow, Audit: recorder}
}

func (uc *DeleteExpense) WithOccurrences(o expRepo.OccurrenceRepository) *DeleteExpense {
	uc.Occurrences = o
	return uc
}

func (uc *DeleteExpense) Execute(ctx context.Context, in DeleteExpenseInput) error {
	now := time.Now().UTC()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		e, err := uc.Expenses.GetByID(tx, in.GymID, in.ExpenseID)
		if err != nil {
			return err
		}
		if e.DeletedAt != nil {
			return nil
		}
		if in.ExpectedVersion > 0 && in.ExpectedVersion != e.Version {
			return sharedDomain.NewBusinessError(expErrors.ErrVersionConflict, "")
		}

		correctionReason, correctionErr := normalizedCorrectionReason(in.CorrectionReason)
		if correctionErr != nil {
			return correctionErr
		}
		in.CorrectionReason = correctionReason
		var linkedMovementID *uuid.UUID
		if uc.CashMovements != nil {
			var mID *uuid.UUID
			if e.CashMovementID != nil {
				mID = e.CashMovementID
			} else {
				m, lookupErr := uc.CashMovements.GetByExpenseID(tx, in.GymID, e.ID)
				switch {
				case lookupErr == nil:
					mID = &m.ID
					e.CashMovementID = &m.ID
				case errors.Is(lookupErr, expErrors.ErrCashMovementNotFound):
					// This expense has no physical drawer movement to reverse.
				default:
					return sharedDomain.NewUnexpectedError(lookupErr)
				}
			}
			if mID != nil {
				m, err := uc.CashMovements.GetByID(tx, in.GymID, *mID)
				if err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if m.ClassificationStatus != "expense" || m.ExpenseID == nil || *m.ExpenseID != e.ID {
					return sharedDomain.NewBusinessError(expErrors.ErrLinkedExpense, "Revisa la salida de caja del gasto.")
				}
				v := m.Version
				originalDate := m.MovementOn
				originalRecordedAt := m.CreatedAt
				originalDrawerID := m.CashDrawerID
				if originalDrawerID == uuid.Nil {
					originalDrawerID = in.GymID
				}
				m.SoftDelete(now)
				if _, err = uc.CashMovements.Update(tx, m, v); err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if uc.Marker != nil {
					if err = uc.Marker.MarkPhysicalCashAdjustment(ctx, tx, CashSessionAdjustmentInput{
						GymID: in.GymID, ActorUserID: in.ActorUserID,
						OperationalDate: originalDate, OriginalRecordedAt: originalRecordedAt,
						DrawerID: originalDrawerID, Reason: in.CorrectionReason,
					}); err != nil {
						return err
					}
				}
				linkedMovementID = &m.ID
			}
		}
		if e.RecurringOccurrenceID != nil {
			if uc.Occurrences == nil {
				return sharedDomain.NewBusinessError(expErrors.ErrLinkedExpense, "")
			}
			o, err := uc.Occurrences.GetByID(tx, in.GymID, *e.RecurringOccurrenceID)
			if err != nil {
				return err
			}
			if o.Status != "paid" || o.ExpenseID == nil || *o.ExpenseID != e.ID {
				return sharedDomain.NewBusinessError(expErrors.ErrLinkedExpense, "")
			}
			ov := o.Version
			if err := o.Reopen(now); err != nil {
				return err
			}
			if _, err := uc.Occurrences.Update(tx, o, ov); err != nil {
				return err
			}
			if err := recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expense_occurrences", o.ID, "reopen", map[string]any{"expense_id": e.ID, "correction_reason": in.CorrectionReason}, now); err != nil {
				return err
			}
			e.DetachRecurringOccurrenceForCorrection()
		}
		e.SoftDelete(now)
		if _, err := uc.Expenses.Update(tx, e); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err := uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "expenses",
			EntityID:    e.ID,
			Action:      audit.ActionDelete,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"amount": e.Amount, "category": e.Category,
				"correction_reason": in.CorrectionReason,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		}); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if linkedMovementID != nil {
			if err := recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", *linkedMovementID, audit.ActionDelete, map[string]any{"expense_id": e.ID}, now); err != nil {
				return err
			}
		}
		return nil
	})
}
