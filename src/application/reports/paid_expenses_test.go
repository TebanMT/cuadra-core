package reports_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/application/reports"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func paidRange() reports.PaidExpensesInput {
	return reports.PaidExpensesInput{GymID: uuid.New(), From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), PageSize: 50}
}
func TestPaidExpensesCombinesCanonicalPurchasesAndExpensesBeforePaging(t *testing.T) {
	in := paidRange()
	r := &fakeReader{}
	for i := 0; i < 201; i++ {
		label := fmt.Sprintf("Renta %03d", i)
		r.expenseRows = append(r.expenseRows, reports.ExpenseRow{ID: uuid.New(), ExpenseDate: in.From, Amount: 1.01, Description: &label})
	}
	id := uuid.New()
	r.inventoryCostRows = []reports.InventoryCostRow{{MovementID: id, ProductID: uuid.New(), ProductName: "Agua", Delta: 3, CostUnit: 0.1, CostTotal: 0.3, OccurredAt: in.To}}
	uow := &snapshotSpyUoW{}
	uc := reports.NewPaidExpenses(r, uow, nil)
	out, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 202 || len(out.Items) != 50 || out.TotalAmount != 203.31 {
		t.Fatalf("invalid page/totals: %+v", out)
	}
	if out.Items[0].Kind != "purchase" || out.Items[0].MovementID != id.String() || out.Items[0].Date != "2026-09-30" {
		t.Fatalf("purchase must use its economic date: %+v", out.Items[0])
	}
	if r.inventoryListLimit != -1 || r.expenseListLimit != -1 || uow.snapshots != 1 || uow.queries != 0 {
		t.Fatalf("must read canonical complete sources in a snapshot: %+v", uow)
	}
	in.Page = 5
	last, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Items) != 2 || last.TotalAmount != out.TotalAmount {
		t.Fatalf("last page: %+v", last)
	}
	in.Page = math.MaxInt
	empty, err := uc.Execute(context.Background(), in)
	if err != nil || len(empty.Items) != 0 || empty.Total != 202 {
		t.Fatalf("out-of-range page: %+v %v", empty, err)
	}
	in.Page = 1
	in.Query = "  aGUA "
	filtered, err := uc.Execute(context.Background(), in)
	if err != nil || filtered.Total != 1 || filtered.TotalAmount != 0.3 {
		t.Fatalf("search must filter total too: %+v %v", filtered, err)
	}
}

type failingPaidReader struct{ fakeReader }

func (*failingPaidReader) ListInventoryCostsBetween(sharedDomain.Transaction, uuid.UUID, string, time.Time, time.Time, int) ([]reports.InventoryCostRow, error) {
	return nil, errors.New("database unavailable")
}
func TestPaidExpensesNeverReturnsPartialTotalOnReadError(t *testing.T) {
	_, err := reports.NewPaidExpenses(&failingPaidReader{}, fakeUoW{}, nil).Execute(context.Background(), paidRange())
	if err == nil {
		t.Fatal("expected error instead of a partial ledger")
	}
}
func TestPaidExpensesRejectsReversedDates(t *testing.T) {
	in := paidRange()
	in.From = in.To.AddDate(0, 0, 1)
	if _, err := reports.NewPaidExpenses(&fakeReader{}, fakeUoW{}, nil).Execute(context.Background(), in); err == nil {
		t.Fatal("expected date validation error")
	}
}
