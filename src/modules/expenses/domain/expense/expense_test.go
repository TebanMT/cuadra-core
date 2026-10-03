package expense

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
)

func validPaidInput() PaidInput {
	return PaidInput{
		ID: uuid.New(), GymID: uuid.New(), CreatedBy: uuid.New(),
		PaidOn: time.Date(2026, 8, 13, 18, 30, 0, 0, time.UTC),
		Amount: 123.45, Category: CategoryUtilities, PaymentMethod: PaymentTransfer,
		Classification: ClassificationVariable, Source: SourceManual, Now: time.Now().UTC(),
	}
}

func TestPaidExpenseValidatesExactMoneyBounds(t *testing.T) {
	for _, amount := range []float64{0, -1, math.NaN(), math.Inf(1), 10_000_000_000, 1.001} {
		in := validPaidInput()
		in.Amount = amount
		if _, err := NewPaid(in); !errors.Is(err, expErrors.ErrInvalidAmount) {
			t.Errorf("amount %v error=%v, want invalid amount", amount, err)
		}
	}
	for _, amount := range []float64{0.01, 9_999_999_999.99} {
		in := validPaidInput()
		in.Amount = amount
		if _, err := NewPaid(in); err != nil {
			t.Errorf("valid amount %v rejected: %v", amount, err)
		}
	}
}

func TestPaidExpenseMeasuresTextInUnicodeCharacters(t *testing.T) {
	in := validPaidInput()
	accepted := strings.Repeat("á", 200)
	in.Description = &accepted
	if _, err := NewPaid(in); err != nil {
		t.Fatalf("200 Unicode characters rejected: %v", err)
	}

	rejected := strings.Repeat("🏋️", 201)
	in.Description = &rejected
	if _, err := NewPaid(in); !errors.Is(err, expErrors.ErrInvalidDescription) {
		t.Fatalf("oversized Unicode description error=%v", err)
	}

	payee := strings.Repeat("ñ", 121)
	in = validPaidInput()
	in.PayeeName = &payee
	if _, err := NewPaid(in); !errors.Is(err, expErrors.ErrInvalidPayee) {
		t.Fatalf("oversized payee error=%v", err)
	}
}

func TestPaidExpenseNormalizesLegacyCategoriesAndRequiresSourceLink(t *testing.T) {
	for legacy, canonical := range map[string]string{
		CategorySalaries:          CategoryPayroll,
		CategoryExternalInventory: CategoryNonInventorySupplies,
	} {
		in := validPaidInput()
		in.Category = legacy
		e, err := NewPaid(in)
		if err != nil || e.Category != canonical {
			t.Errorf("legacy %q => %q err=%v, want %q", legacy, e.Category, err, canonical)
		}
	}

	in := validPaidInput()
	in.Source = SourceRecurring
	if _, err := NewPaid(in); !errors.Is(err, expErrors.ErrInvalidSource) {
		t.Fatalf("recurring without occurrence error=%v", err)
	}
	occurrenceID := uuid.New()
	in.RecurringOccurrenceID = &occurrenceID
	e, err := NewPaid(in)
	if err != nil || e.RecurringOccurrenceID == nil || *e.RecurringOccurrenceID != occurrenceID {
		t.Fatalf("linked recurring expense=%+v err=%v", e, err)
	}
}

func TestPaidOnIsDateOnlyAndStorageAliasMatches(t *testing.T) {
	in := validPaidInput()
	e, err := NewPaid(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.PaidOn.Format(time.RFC3339); got != "2026-08-13T00:00:00Z" {
		t.Fatalf("paid_on=%s", got)
	}
	if !e.PaidOn.Equal(e.ExpenseDate) {
		t.Fatalf("paid_on %s differs from storage alias %s", e.PaidOn, e.ExpenseDate)
	}
}

func TestPaidFromSeparatesDrawerFromPaymentMethod(t *testing.T) {
	in := validPaidInput()
	in.PaymentMethod = PaymentCash
	in.PaidFrom = PaidFromGymFund
	e, err := NewPaid(in)
	if err != nil || e.PaidFrom != PaidFromGymFund {
		t.Fatalf("cash paid from gym fund = %+v, err=%v", e, err)
	}
	in.PaymentMethod = PaymentTransfer
	in.PaidFrom = PaidFromCashRegister
	if _, err = NewPaid(in); !errors.Is(err, expErrors.ErrInvalidPaidFrom) {
		t.Fatalf("drawer with transfer error=%v, want invalid paid_from", err)
	}
}

func TestPaidFromCashDrawerIsCanonicalWireAlias(t *testing.T) {
	in := validPaidInput()
	in.PaymentMethod = PaymentCash
	in.PaidFrom = PaidFromCashDrawer
	e, err := NewPaid(in)
	if err != nil {
		t.Fatal(err)
	}
	if e.PaidFrom != PaidFromCashRegister {
		t.Fatalf("stored paid_from=%q, want legacy-normalized %q", e.PaidFrom, PaidFromCashRegister)
	}
	if got := PaidFromWire(e.PaidFrom); got != PaidFromCashDrawer {
		t.Fatalf("wire paid_from=%q, want %q", got, PaidFromCashDrawer)
	}
}
