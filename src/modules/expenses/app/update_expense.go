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
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type UpdateExpenseInput struct {
	GymID            uuid.UUID
	ActorUserID      uuid.UUID
	ExpenseID        uuid.UUID
	ExpenseDate      time.Time
	Amount           float64
	Category         string
	Description      *string
	PaymentMethod    string
	PaidFrom         string
	ExpectedVersion  int
	PayeeName        *string
	Reference        *string
	Classification   string
	CorrectionReason string
	CashDrawerID     *uuid.UUID
}

type UpdateExpense struct {
	Occurrences   expRepo.OccurrenceRepository
	Expenses      expRepo.ExpenseRepository
	UoW           sharedDomain.UnitOfWork
	Audit         audit.Recorder
	CashMovements expRepo.CashMovementRepository
	Policy        OperationalPolicy
	Marker        CashSessionAdjustmentMarker
	DrawerCatalog CashDrawerValidator
}

func (uc *UpdateExpense) WithOccurrences(o expRepo.OccurrenceRepository) *UpdateExpense {
	uc.Occurrences = o
	return uc
}

func (uc *UpdateExpense) WithOperational(c expRepo.CashMovementRepository, p OperationalPolicy) *UpdateExpense {
	uc.CashMovements = c
	uc.Policy = p
	return uc
}

func (uc *UpdateExpense) WithCashSessionMarker(marker CashSessionAdjustmentMarker) *UpdateExpense {
	uc.Marker = marker
	return uc
}

func (uc *UpdateExpense) WithCashDrawerValidator(validator CashDrawerValidator) *UpdateExpense {
	uc.DrawerCatalog = validator
	return uc
}

func NewUpdateExpense(expenses expRepo.ExpenseRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateExpense {
	return &UpdateExpense{Expenses: expenses, UoW: uow, Audit: recorder}
}

func (uc *UpdateExpense) Execute(ctx context.Context, in UpdateExpenseInput) error {
	if expenseDomain.NormalizePaidFrom(in.PaidFrom) == "" {
		return sharedDomain.NewValidationError(expErrors.ErrInvalidPaidFrom)
	}
	now := time.Now().UTC()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		e, err := uc.Expenses.GetByID(tx, in.GymID, in.ExpenseID)
		if err != nil {
			return err
		}
		if in.ExpectedVersion > 0 && in.ExpectedVersion != e.Version {
			return sharedDomain.NewBusinessError(expErrors.ErrVersionConflict, "")
		}

		if e.RecurringOccurrenceID != nil {
			if uc.Occurrences == nil {
				return sharedDomain.NewUnexpectedError(errors.New("occurrence repository missing"))
			}
			o, getErr := uc.Occurrences.GetByID(tx, in.GymID, *e.RecurringOccurrenceID)
			if getErr != nil {
				return getErr
			}
			if o.Status != "paid" || o.ExpenseID == nil || *o.ExpenseID != e.ID {
				return sharedDomain.NewBusinessError(expErrors.ErrLinkedExpense, "El pago programado cambió. Vuelve a abrirlo.")
			}
		}

		correctionReason, correctionErr := normalizedCorrectionReason(in.CorrectionReason)
		if correctionErr != nil {
			return correctionErr
		}
		in.CorrectionReason = correctionReason
		if err := uc.Policy.validatePaidDate(tx, in.GymID, in.ExpenseDate, now); err != nil {
			return err
		}
		before := map[string]any{
			"expense_date":   e.ExpenseDate.Format("2006-01-02"),
			"amount":         e.Amount,
			"category":       e.Category,
			"payment_method": e.PaymentMethod,
			"paid_from":      e.PaidFrom,
			"description":    e.Description,
		}
		var linkedMovement *cashDomain.CashMovement
		var linkedMovementAction string
		// Older/synchronized rows can temporarily have only the reverse side of
		// the 1:1 relation (cash_movements.expense_id). Recover it before deciding
		// whether to update or create a drawer movement; creating here would hit
		// the unique expense_id constraint and, more importantly, double-count cash.
		if e.CashMovementID == nil && uc.CashMovements != nil {
			m, lookupErr := uc.CashMovements.GetByExpenseID(tx, in.GymID, e.ID)
			switch {
			case lookupErr == nil:
				linkedMovement = m
				e.CashMovementID = &m.ID
			case errors.Is(lookupErr, expErrors.ErrCashMovementNotFound):
				// A cash-register update below will create the missing movement.
			default:
				return sharedDomain.NewUnexpectedError(lookupErr)
			}
		}
		if err := e.Update(in.ExpenseDate, in.Amount, in.Category, in.PaymentMethod, in.Description, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if err := e.SetPaidFrom(in.PaidFrom); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if err := e.SetMetadata(in.PayeeName, in.Reference, in.Classification, e.Source, e.RecurringOccurrenceID, e.CashMovementID); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if err := validateDrawerAppliesToExpense(e, in.CashDrawerID); err != nil {
			return err
		}
		if uc.CashMovements != nil {
			if e.CashMovementID != nil {
				m := linkedMovement
				if m == nil {
					m, err = uc.CashMovements.GetByID(tx, in.GymID, *e.CashMovementID)
					if err != nil {
						return sharedDomain.NewUnexpectedError(err)
					}
				}
				if m.MovementType != cashDomain.CashOut || m.ClassificationStatus != cashDomain.AsExpense || m.ExpenseID == nil || *m.ExpenseID != e.ID {
					return sharedDomain.NewBusinessError(expErrors.ErrLinkedExpense, "La salida de caja cambió. Vuelve a abrir el gasto.")
				}
				mv := m.Version
				originalDate := m.MovementOn
				originalRecordedAt := m.CreatedAt
				originalDrawerID := m.CashDrawerID
				if originalDrawerID == uuid.Nil {
					originalDrawerID = in.GymID
				}
				destinationDrawerID := originalDrawerID
				if e.PaidFrom == expenseDomain.PaidFromCashRegister && in.CashDrawerID != nil && *in.CashDrawerID != uuid.Nil {
					destinationDrawerID = *in.CashDrawerID
					if err = validateRequestedCashDrawer(tx, uc.DrawerCatalog, in.GymID, destinationDrawerID); err != nil {
						return err
					}
				}
				physicalChanged := e.PaidFrom != expenseDomain.PaidFromCashRegister ||
					!dateOnly(originalDate).Equal(dateOnly(in.ExpenseDate)) || m.Amount != in.Amount ||
					originalDrawerID != destinationDrawerID
				if e.PaidFrom == expenseDomain.PaidFromCashRegister {
					if err = m.Update(in.ExpenseDate, in.Amount, cashDomain.CashOut, expenseCashReason(e.Description), now); err != nil {
						return sharedDomain.NewValidationError(err)
					}
					m.WithCashDrawer(destinationDrawerID)
				} else {
					m.SoftDelete(now)
					e.CashMovementID = nil
					linkedMovementAction = audit.ActionDelete
				}
				if _, err = uc.CashMovements.Update(tx, m, mv); err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				if physicalChanged && uc.Marker != nil {
					if err = uc.Marker.MarkPhysicalCashAdjustment(ctx, tx, CashSessionAdjustmentInput{
						GymID: in.GymID, ActorUserID: in.ActorUserID,
						OperationalDate: originalDate, OriginalRecordedAt: originalRecordedAt,
						DrawerID: originalDrawerID, Reason: in.CorrectionReason,
					}); err != nil {
						return err
					}
					if e.PaidFrom == expenseDomain.PaidFromCashRegister &&
						(!dateOnly(originalDate).Equal(dateOnly(m.MovementOn)) || originalDrawerID != destinationDrawerID) {
						if err = uc.Marker.MarkPhysicalCashAdjustment(ctx, tx, CashSessionAdjustmentInput{
							GymID: in.GymID, ActorUserID: in.ActorUserID,
							OperationalDate: m.MovementOn, DrawerID: destinationDrawerID,
							Reason: in.CorrectionReason,
						}); err != nil {
							return err
						}
					}
				}
				linkedMovement = m
				if m.DeletedAt == nil {
					d := m.CashDrawerID
					e.CashDrawerID = &d
				} else {
					e.CashDrawerID = nil
				}
				if linkedMovementAction == "" {
					linkedMovementAction = audit.ActionUpdate
				}
			} else if e.PaidFrom == expenseDomain.PaidFromCashRegister {
				drawerID := effectiveCashDrawerID(in.GymID, in.CashDrawerID)
				if err := validateRequestedCashDrawer(tx, uc.DrawerCatalog, in.GymID, drawerID); err != nil {
					return err
				}
				m, err := newExpenseCashMovement(expenseCashMovementID(e.ID), in.GymID, in.ActorUserID, in.ExpenseDate, in.Amount, e.Description, drawerID, now)
				if err != nil {
					return sharedDomain.NewValidationError(err)
				}
				if _, err = uc.CashMovements.Create(tx, m); err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				e.CashMovementID = &m.ID
				mv := m.Version
				if err = m.Classify(&e.ID, cashDomain.AsExpense, now); err != nil {
					return sharedDomain.NewValidationError(err)
				}
				if _, err = uc.CashMovements.Update(tx, m, mv); err != nil {
					return sharedDomain.NewUnexpectedError(err)
				}
				linkedMovement = m
				d := m.CashDrawerID
				e.CashDrawerID = &d
				linkedMovementAction = audit.ActionCreate
			}
		}
		if e.Source == expenseDomain.SourceCashMovement && e.CashMovementID == nil {
			if err := e.SetMetadata(e.PayeeName, e.Reference, e.Classification, expenseDomain.SourceManual, nil, nil); err != nil {
				return sharedDomain.NewValidationError(err)
			}
		}

		if _, err := uc.Expenses.Update(tx, e); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err := uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "expenses",
			EntityID:    e.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"before": before,
				"after": map[string]any{
					"expense_date":   e.ExpenseDate.Format("2006-01-02"),
					"amount":         e.Amount,
					"category":       e.Category,
					"payment_method": e.PaymentMethod,
					"paid_from":      e.PaidFrom,
					"description":    e.Description,
					"cash_drawer_id": func() any {
						if linkedMovement == nil || linkedMovement.DeletedAt != nil {
							return nil
						}
						return linkedMovement.CashDrawerID
					}(),
				},
				"correction_reason": in.CorrectionReason,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		}); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if linkedMovement != nil {
			if err := recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", linkedMovement.ID, linkedMovementAction, map[string]any{"expense_id": e.ID, "amount": linkedMovement.Amount, "deleted_at": linkedMovement.DeletedAt, "cash_drawer_id": linkedMovement.CashDrawerID}, now); err != nil {
				return err
			}
		}
		return nil
	})
}
