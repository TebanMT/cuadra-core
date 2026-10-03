package controllers

import (
	"testing"
	"time"

	"github.com/google/uuid"

	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
)

func TestExpenseWireNeverReturnsLegacyCashRegister(t *testing.T) {
	now := time.Now().UTC()
	e := &expenseDomain.Expense{
		ID: uuid.New(), GymID: uuid.New(), CreatedBy: uuid.New(), Version: 1,
		ExpenseDate: now, PaidOn: now, PaidFrom: expenseDomain.PaidFromCashRegister,
		CreatedAt: now, UpdatedAt: now,
	}
	if got := toExpenseResp(e).PaidFrom; got != expenseDomain.PaidFromCashDrawer {
		t.Fatalf("paid_from=%q, want canonical %q", got, expenseDomain.PaidFromCashDrawer)
	}
}
