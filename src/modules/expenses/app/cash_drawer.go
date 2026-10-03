package app

import (
	"errors"
	"math"

	"github.com/google/uuid"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CashDrawerValidator is the narrow seam used by expenses to validate a
// physical cash location without importing the billing cash-drawer catalog.
// billing/app.CashDrawers implements it.
type CashDrawerValidator interface {
	ValidateActiveCashDrawer(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID) error
}

func effectiveCashDrawerID(gymID uuid.UUID, requested *uuid.UUID) uuid.UUID {
	if requested != nil && *requested != uuid.Nil {
		return *requested
	}
	return gymID
}

func validateRequestedCashDrawer(tx sharedDomain.Transaction, validator CashDrawerValidator,
	gymID uuid.UUID, drawerID uuid.UUID) error {
	if validator == nil {
		return nil
	}
	return validator.ValidateActiveCashDrawer(tx, gymID, drawerID)
}

func validateDrawerAppliesToExpense(e *expenseDomain.Expense, requested *uuid.UUID) error {
	if requested != nil && *requested != uuid.Nil && e.PaidFrom != expenseDomain.PaidFromCashRegister {
		return sharedDomain.NewValidationError(expErrors.ErrInvalidPaidFrom)
	}
	return nil
}

func expenseCashMovementID(expenseID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("expense-cash:"+expenseID.String()))
}

func sameExpensePayload(a, b *expenseDomain.Expense) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return dateOnly(a.PaidOn).Equal(dateOnly(b.PaidOn)) &&
		math.Round(a.Amount*100) == math.Round(b.Amount*100) &&
		a.Category == b.Category && a.PaymentMethod == b.PaymentMethod &&
		a.PaidFrom == b.PaidFrom && a.Classification == b.Classification &&
		a.Source == b.Source && sameOptionalText(a.PayeeName, b.PayeeName) &&
		sameOptionalText(a.Description, b.Description) &&
		sameOptionalText(a.Reference, b.Reference) &&
		sameOptionalUUID(a.RecurringOccurrenceID, b.RecurringOccurrenceID)
}

func sameOptionalText(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameOptionalUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func expenseUsesDrawer(tx sharedDomain.Transaction, movements expRepo.CashMovementRepository,
	e *expenseDomain.Expense, drawerID uuid.UUID) (bool, error) {
	if e.PaidFrom != expenseDomain.PaidFromCashRegister {
		return e.CashMovementID == nil, nil
	}
	if e.CashMovementID == nil || movements == nil {
		return false, nil
	}
	m, err := movements.GetByID(tx, e.GymID, *e.CashMovementID)
	if err != nil {
		if errors.Is(err, expErrors.ErrCashMovementNotFound) {
			return false, nil
		}
		return false, err
	}
	actual := m.CashDrawerID
	if actual == uuid.Nil {
		actual = e.GymID
	}
	return actual == drawerID && m.ExpenseID != nil && *m.ExpenseID == e.ID, nil
}
