package purchase

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPurchaseNormalizesLegacyCashRegisterToCanonicalCashDrawer(t *testing.T) {
	cost := 50.0
	day := time.Now().UTC()
	cashMovementID := uuid.New()
	p, err := New(Input{
		ID: uuid.New(), GymID: uuid.New(), StockMovementID: uuid.New(), ProductID: uuid.New(), CreatedBy: uuid.New(),
		Quantity: 2, UnitCost: &cost, Status: StatusPaid, PaidOn: &day,
		PaymentMethod: MethodCash, PaidFrom: "cash_register", CashMovementID: &cashMovementID,
		IdempotencyKey: "purchase:wire-alias", Now: day,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.PaidFrom == nil || *p.PaidFrom != PaidFromCashDrawer {
		t.Fatalf("paid_from=%v, want %q", p.PaidFrom, PaidFromCashDrawer)
	}
}
