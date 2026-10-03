package reports

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

type cashCommandTestTx struct{}

func (cashCommandTestTx) Execute(fn func(sharedDomain.Transaction) error) error {
	return fn(cashCommandTestTx{})
}

type cashCommandTestUoW struct{}

func (cashCommandTestUoW) Begin(context.Context) (sharedDomain.Transaction, error) {
	return cashCommandTestTx{}, nil
}
func (cashCommandTestUoW) Commit(sharedDomain.Transaction) error   { return nil }
func (cashCommandTestUoW) Rollback(sharedDomain.Transaction) error { return nil }
func (cashCommandTestUoW) Query(context.Context) (sharedDomain.Transaction, error) {
	return cashCommandTestTx{}, nil
}
func (cashCommandTestUoW) Command(_ context.Context, fn func(sharedDomain.Transaction) error) error {
	return fn(cashCommandTestTx{})
}

type cashCommandGymRepo struct {
	gymRepo.GymRepository
	gym *gymDomain.Gym
	err error
}

func (r cashCommandGymRepo) GetByID(sharedDomain.Transaction, uuid.UUID) (*gymDomain.Gym, error) {
	return r.gym, r.err
}

type cashCommandEventSpy struct {
	billingRepo.CashCloseEventRepository
	touched bool
}

func (r *cashCommandEventSpy) GetByID(sharedDomain.Transaction, uuid.UUID, uuid.UUID) (*cashCloseDomain.CashCloseEvent, error) {
	r.touched = true
	return nil, nil
}

func (r *cashCommandEventSpy) ListByDate(sharedDomain.Transaction, uuid.UUID, time.Time) ([]*cashCloseDomain.CashCloseEvent, error) {
	r.touched = true
	return nil, nil
}

func TestAppendDrawerExpensesExcludesGymFundAndExternal(t *testing.T) {
	description := "Limpieza"
	out := &CashCloseReportOutput{
		Expenses:         []CashCloseExpenseEntry{},
		ExpensesByMethod: map[string]float64{},
	}
	appendDrawerExpenses(out, []*expenseDomain.Expense{
		{ID: uuid.New(), Amount: 100, Category: expenseDomain.CategoryMaintenance, PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister, Description: &description},
		{ID: uuid.New(), Amount: 900, Category: expenseDomain.CategoryRent, PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromGymFund},
		{ID: uuid.New(), Amount: 500, Category: expenseDomain.CategoryUtilities, PaymentMethod: expenseDomain.PaymentTransfer, PaidFrom: expenseDomain.PaidFromExternal},
	}, nil, false)

	if len(out.Expenses) != 1 {
		t.Fatalf("drawer expenses = %d, want 1", len(out.Expenses))
	}
	if out.ExpensesTotal != 100 || out.ExpensesByMethod[expenseDomain.PaymentCash] != 100 {
		t.Fatalf("drawer totals = %.2f / %+v, want 100 cash", out.ExpensesTotal, out.ExpensesByMethod)
	}
	if out.Expenses[0].Description == nil || *out.Expenses[0].Description != description {
		t.Fatalf("drawer expense detail = %+v", out.Expenses[0])
	}
}

func TestValidateCashOperationalDate_UsesGymLocalTodayAndAllowsBackdating(t *testing.T) {
	// 25-ago 02:00 UTC todavía es 24-ago 20:00 en Ciudad de México. Una
	// comparación contra el día UTC permitiría cerrar por adelantado el 25.
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	today := tz.LocalToday("America/Mexico_City", now)

	tests := []struct {
		name string
		day  time.Time
		want error
	}{
		{name: "backdated", day: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)},
		{name: "gym local today", day: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)},
		{name: "tomorrow in gym", day: time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC), want: billingErrors.ErrCashCloseFutureDate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateCashOperationalDate(tt.day, today)
			if !errors.Is(got, tt.want) {
				t.Fatalf("validateCashOperationalDate(%s, %s) error = %v, want %v",
					tt.day.Format("2006-01-02"), today.Format("2006-01-02"), got, tt.want)
			}
		})
	}
}

func TestCashCommands_FailClosedOnGymCalendarErrorBeforeTouchingLedger(t *testing.T) {
	originalNow := nowUTC
	nowUTC = func() time.Time { return time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowUTC = originalNow })

	repoFailure := errors.New("gym repository unavailable")
	for _, command := range []string{"close", "reopen"} {
		t.Run(command, func(t *testing.T) {
			events := &cashCommandEventSpy{}
			uc := &CashClose{
				UoW:    cashCommandTestUoW{},
				Gyms:   cashCommandGymRepo{err: repoFailure},
				Events: events,
			}

			var err error
			switch command {
			case "close":
				_, err = uc.Close(context.Background(), CashCloseInput{GymID: uuid.New()})
			case "reopen":
				err = uc.Reopen(context.Background(), CashReopenInput{
					GymID: uuid.New(), Reason: stringPtr("captura equivocada"),
				})
			}
			if !errors.Is(err, repoFailure) {
				t.Fatalf("%s error = %v, want repository failure", command, err)
			}
			var custom sharedDomain.CustomError
			if !errors.As(err, &custom) || custom.ErrorCode != sharedDomain.CodeUnexpected {
				t.Fatalf("%s error = %#v, want unexpected infrastructure error", command, err)
			}
			if events.touched {
				t.Fatalf("%s touched the cash ledger after calendar lookup failed", command)
			}
		})
	}
}

func TestCashCommands_RejectTomorrowInGymAtCDMXEightPM(t *testing.T) {
	originalNow := nowUTC
	nowUTC = func() time.Time { return time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowUTC = originalNow })

	tomorrowLocal := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	for _, command := range []string{"close", "reopen"} {
		t.Run(command, func(t *testing.T) {
			events := &cashCommandEventSpy{}
			uc := &CashClose{
				UoW: cashCommandTestUoW{},
				Gyms: cashCommandGymRepo{gym: &gymDomain.Gym{
					ID: uuid.New(), Timezone: "America/Mexico_City",
				}},
				Events: events,
			}

			var err error
			switch command {
			case "close":
				_, err = uc.Close(context.Background(), CashCloseInput{GymID: uuid.New(), Date: tomorrowLocal})
			case "reopen":
				err = uc.Reopen(context.Background(), CashReopenInput{
					GymID: uuid.New(), Date: tomorrowLocal, Reason: stringPtr("captura equivocada"),
				})
			}
			if !errors.Is(err, billingErrors.ErrCashCloseFutureDate) {
				t.Fatalf("%s error = %v, want future local date", command, err)
			}
			if events.touched {
				t.Fatalf("%s touched the cash ledger for a future local date", command)
			}
		})
	}
}

func TestCashCommandLocalTodayAndTZ_ValidatesConfiguredTimezone(t *testing.T) {
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	gymID := uuid.New()

	today, timezone, err := cashCommandLocalTodayAndTZ(cashCommandTestTx{}, nil, gymID, now)
	if err != nil || !today.Equal(time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)) || timezone != "" {
		t.Fatalf("nil-repo compatibility = %s/%q/%v, want UTC fallback", today, timezone, err)
	}

	_, _, err = cashCommandLocalTodayAndTZ(cashCommandTestTx{}, cashCommandGymRepo{
		gym: &gymDomain.Gym{ID: gymID, Timezone: "Mars/Olympus_Mons"},
	}, gymID, now)
	if err == nil {
		t.Fatal("invalid configured timezone accepted for cash mutation")
	}
}

func stringPtr(value string) *string { return &value }
