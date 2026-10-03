package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CreateExpenseInput — captura de un gasto general. Si Description está
// vacío, viaja como nil. ExpenseDate llega ya parseada por el controller.
type CreateExpenseInput struct {
	GymID          uuid.UUID
	ActorUserID    uuid.UUID
	ExpenseDate    time.Time
	Amount         float64
	Category       string
	Description    *string
	PaymentMethod  string
	PaidFrom       string
	PayeeName      *string
	Reference      *string
	Classification string
	CashDrawerID   *uuid.UUID
	IdempotencyKey string
}

type CreateExpenseOutput struct {
	ExpenseID    uuid.UUID
	Amount       float64
	Category     string
	CashDrawerID *uuid.UUID
}

type CreateExpense struct {
	Expenses      expRepo.ExpenseRepository
	UoW           sharedDomain.UnitOfWork
	Audit         audit.Recorder
	CashMovements expRepo.CashMovementRepository
	Policy        OperationalPolicy
	DrawerCatalog CashDrawerValidator
}

func (uc *CreateExpense) WithOperational(c expRepo.CashMovementRepository, p OperationalPolicy) *CreateExpense {
	uc.CashMovements = c
	uc.Policy = p
	return uc
}

func (uc *CreateExpense) WithCashDrawerValidator(validator CashDrawerValidator) *CreateExpense {
	uc.DrawerCatalog = validator
	return uc
}

func NewCreateExpense(expenses expRepo.ExpenseRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreateExpense {
	return &CreateExpense{Expenses: expenses, UoW: uow, Audit: recorder}
}

func (uc *CreateExpense) Execute(ctx context.Context, in CreateExpenseInput) (*CreateExpenseOutput, error) {
	if expenseDomain.NormalizePaidFrom(in.PaidFrom) == "" {
		return nil, sharedDomain.NewValidationError(expErrors.ErrInvalidPaidFrom)
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if len(key) > 120 {
		return nil, sharedDomain.NewValidationError(expErrors.ErrIdempotencyKeyInvalid)
	}
	now := time.Now().UTC()
	var out CreateExpenseOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		expenseID := uuid.New()
		if key != "" {
			expenseID = uuid.NewSHA1(in.GymID, []byte("expense:"+key))
		}
		e, err := expenseDomain.NewPaid(expenseDomain.PaidInput{ID: expenseID, GymID: in.GymID, CreatedBy: in.ActorUserID, PaidOn: in.ExpenseDate, Amount: in.Amount, Category: in.Category, PaymentMethod: in.PaymentMethod, PaidFrom: in.PaidFrom, Description: in.Description, PayeeName: in.PayeeName, Reference: in.Reference, Classification: in.Classification, Source: expenseDomain.SourceManual, Now: now})
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if err = validateDrawerAppliesToExpense(e, in.CashDrawerID); err != nil {
			return err
		}
		drawerID := effectiveCashDrawerID(in.GymID, in.CashDrawerID)
		if key != "" {
			stored, lookupErr := uc.Expenses.GetByID(tx, in.GymID, expenseID)
			if lookupErr == nil {
				same := sameExpensePayload(stored, e)
				if same {
					same, lookupErr = expenseUsesDrawer(tx, uc.CashMovements, stored, drawerID)
				}
				if lookupErr != nil {
					return sharedDomain.NewUnexpectedError(lookupErr)
				}
				if !same {
					return sharedDomain.NewBusinessError(expErrors.ErrIdempotencyKeyConflict, "")
				}
				out = CreateExpenseOutput{ExpenseID: stored.ID, Amount: stored.Amount, Category: stored.Category, CashDrawerID: stored.CashDrawerID}
				return nil
			}
			if !errors.Is(lookupErr, expErrors.ErrExpenseNotFound) {
				return sharedDomain.NewUnexpectedError(lookupErr)
			}
		}
		if err = uc.Policy.validatePaidDate(tx, in.GymID, in.ExpenseDate, now); err != nil {
			return err
		}
		var linkedMovement *cashDomain.CashMovement
		if e.PaidFrom == expenseDomain.PaidFromCashRegister {
			if uc.CashMovements == nil {
				return sharedDomain.NewUnexpectedError(errors.New("cash movement repository is not configured"))
			}
			if err = validateRequestedCashDrawer(tx, uc.DrawerCatalog, in.GymID, drawerID); err != nil {
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
			if _, err := uc.Expenses.Create(tx, e); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			v := m.Version
			if err = m.Classify(&e.ID, "expense", now); err != nil {
				return err
			}
			if _, err = uc.CashMovements.Update(tx, m, v); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			linkedMovement = m
			d := m.CashDrawerID
			e.CashDrawerID = &d
		} else if _, err := uc.Expenses.Create(tx, e); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if err := uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "expenses",
			EntityID:    e.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"expense_date":   e.ExpenseDate.Format("2006-01-02"),
				"amount":         e.Amount,
				"category":       e.Category,
				"payment_method": e.PaymentMethod,
				"paid_from":      e.PaidFrom,
				"description":    e.Description,
				"cash_drawer_id": func() any {
					if linkedMovement == nil {
						return nil
					}
					return linkedMovement.CashDrawerID
				}(),
				"idempotency_key": key,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		}); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if linkedMovement != nil {
			if err := recordFinancial(ctx, uc.Audit, tx, in.GymID, in.ActorUserID, "cash_movements", linkedMovement.ID, audit.ActionCreate, map[string]any{"expense_id": e.ID, "amount": linkedMovement.Amount, "classification_status": linkedMovement.ClassificationStatus, "cash_drawer_id": linkedMovement.CashDrawerID}, now); err != nil {
				return err
			}
		}
		out = CreateExpenseOutput{ExpenseID: e.ID, Amount: e.Amount, Category: e.Category, CashDrawerID: e.CashDrawerID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func newExpenseCashMovement(id, gymID, actor uuid.UUID, day time.Time, amount float64, desc *string, drawerID uuid.UUID, now time.Time) (*cashDomain.CashMovement, error) {
	m, err := cashDomain.New(id, gymID, actor, day, amount, cashDomain.CashOut, expenseCashReason(desc), now)
	if err != nil {
		return nil, err
	}
	return m.WithCashDrawer(drawerID), nil
}

func expenseCashReason(desc *string) string {
	if desc != nil && *desc != "" {
		return *desc
	}
	return "Gasto pagado"
}
