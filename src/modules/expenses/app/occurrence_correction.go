package app

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	recurringDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/recurring"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ReopenExpenseOccurrenceInput struct {
	GymID, ActorUserID, OccurrenceID uuid.UUID
	ActorRole                        string
	ExpectedVersion                  int
	CorrectionReason                 string
}

// ReopenExpenseOccurrence is the coordinated correction path for a due that
// was paid or skipped by mistake. Paid records become tombstones, any linked
// physical outflow is tombstoned in the same transaction, and the occurrence
// returns to pending so it can be captured correctly again.
type ReopenExpenseOccurrence struct {
	Occurrences expRepo.OccurrenceRepository
	Expenses    expRepo.ExpenseRepository
	Movements   expRepo.CashMovementRepository
	UoW         sharedDomain.UnitOfWork
	Audit       audit.Recorder
	Marker      CashSessionAdjustmentMarker
}

func NewReopenExpenseOccurrence(o expRepo.OccurrenceRepository, e expRepo.ExpenseRepository,
	m expRepo.CashMovementRepository, u sharedDomain.UnitOfWork, a audit.Recorder) *ReopenExpenseOccurrence {
	return &ReopenExpenseOccurrence{Occurrences: o, Expenses: e, Movements: m, UoW: u, Audit: a}
}

func (uc *ReopenExpenseOccurrence) WithCashSessionMarker(marker CashSessionAdjustmentMarker) *ReopenExpenseOccurrence {
	uc.Marker = marker
	return uc
}

func (uc *ReopenExpenseOccurrence) Execute(ctx context.Context, in ReopenExpenseOccurrenceInput) (*recurringDomain.Occurrence, error) {
	if in.ActorRole != "owner" {
		return nil, sharedDomain.NewBusinessError(expErrors.ErrCorrectionOwnerRequired, "")
	}
	reason := strings.TrimSpace(in.CorrectionReason)
	if utf8.RuneCountInString(reason) < 3 || utf8.RuneCountInString(reason) > 200 {
		return nil, sharedDomain.NewValidationError(expErrors.ErrCorrectionReasonRequired)
	}
	var out *recurringDomain.Occurrence
	now := time.Now().UTC()
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		o, err := uc.Occurrences.GetByID(tx, in.GymID, in.OccurrenceID)
		if err != nil {
			return err
		}
		if o.Status == recurringDomain.Pending {
			out = o
			return nil
		}
		if in.ExpectedVersion > 0 && in.ExpectedVersion != o.Version {
			return sharedDomain.NewBusinessError(expErrors.ErrVersionConflict, "")
		}
		previousStatus := o.Status
		var previousExpenseID *uuid.UUID
		if o.Status == recurringDomain.Paid {
			if o.ExpenseID == nil {
				return sharedDomain.NewUnexpectedError(errors.New("paid occurrence has no expense"))
			}
			e, getErr := uc.Expenses.GetByID(tx, in.GymID, *o.ExpenseID)
			if getErr != nil {
				return getErr
			}
			if e.Source != expenseDomain.SourceRecurring || e.RecurringOccurrenceID == nil || *e.RecurringOccurrenceID != o.ID {
				return sharedDomain.NewBusinessError(expErrors.ErrLinkedExpense, "")
			}
			expenseID := e.ID
			previousExpenseID = &expenseID
			if e.CashMovementID != nil {
				if uc.Movements == nil {
					return sharedDomain.NewUnexpectedError(errors.New("cash movement repository is not configured"))
				}
				m, movementErr := uc.Movements.GetByID(tx, in.GymID, *e.CashMovementID)
				if movementErr != nil {
					return sharedDomain.NewUnexpectedError(movementErr)
				}
				if m.ClassificationStatus != cashDomain.AsExpense || m.ExpenseID == nil || *m.ExpenseID != e.ID {
					return sharedDomain.NewUnexpectedError(errors.New("recurring expense cash movement link is inconsistent"))
				}
				movementVersion := m.Version
				movementOn, movementCreatedAt := m.MovementOn, m.CreatedAt
				drawerID := m.CashDrawerID
				if drawerID == uuid.Nil {
					drawerID = in.GymID
				}
				m.SoftDelete(now)
				if _, movementErr = uc.Movements.Update(tx, m, movementVersion); movementErr != nil {
					return sharedDomain.NewUnexpectedError(movementErr)
				}
				if uc.Marker != nil {
					if movementErr = uc.Marker.MarkPhysicalCashAdjustment(ctx, tx, CashSessionAdjustmentInput{
						GymID: in.GymID, ActorUserID: in.ActorUserID, OperationalDate: movementOn,
						OriginalRecordedAt: movementCreatedAt, DrawerID: drawerID, Reason: reason,
					}); movementErr != nil {
						return movementErr
					}
				}
				if movementErr = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", m.ID,
					audit.ActionDelete, map[string]any{"expense_id": e.ID, "correction_reason": reason}, now); movementErr != nil {
					return movementErr
				}
			}
			expenseVersion := e.Version
			e.DetachRecurringOccurrenceForCorrection()
			e.SoftDelete(now)
			if e.Version != expenseVersion+1 {
				return sharedDomain.NewUnexpectedError(errors.New("recurring expense correction did not advance version"))
			}
			if _, getErr = uc.Expenses.Update(tx, e); getErr != nil {
				return sharedDomain.NewUnexpectedError(getErr)
			}
			if getErr = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expenses", e.ID,
				audit.ActionDelete, map[string]any{"recurring_occurrence_id": o.ID, "correction_reason": reason}, now); getErr != nil {
				return getErr
			}
		}
		occurrenceVersion := o.Version
		if err = o.Reopen(now); err != nil {
			return sharedDomain.NewBusinessError(err, "")
		}
		if _, err = uc.Occurrences.Update(tx, o, occurrenceVersion); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err = recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "expense_occurrences", o.ID,
			"reopen", map[string]any{"status_before": previousStatus, "status_after": o.Status,
				"previous_expense_id": previousExpenseID, "correction_reason": reason}, now); err != nil {
			return err
		}
		out = o
		return nil
	})
	return out, err
}
