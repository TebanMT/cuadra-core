package reports_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/reports"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// fakeReader returns canned values keyed by the call. Lets us assert the
// dashboard composition without setting up Postgres.
type fakeReader struct {
	activeNow, activePrev int
	incomeNow, incomePrev float64
	expiringWeek          int
	recoverable           int
	todayCash             map[string]float64
	series                []reports.DailyIncome

	expiringSoon       []reports.MemberExpiringRow
	expiredRecoverable []reports.MemberExpiredRow
	inactiveInvol      []reports.MemberInactiveRow
	lowStock           []reports.ProductLowStockRow
	pendingBalances    []reports.PendingBalanceRow
	birthdays          []reports.MemberBirthdayRow

	exportMembers  []reports.MemberExportRow
	exportPayments []reports.PaymentExportRow
	exportSales    []reports.SaleExportRow

	activeCalls int
	incomeCalls int
}

func (r *fakeReader) CountActiveMembers(_ sharedDomain.Transaction, _ uuid.UUID, t time.Time) (int, error) {
	r.activeCalls++
	if r.activeCalls == 1 {
		return r.activeNow, nil
	}
	return r.activePrev, nil
}
func (r *fakeReader) SumPaymentsBetween(_ sharedDomain.Transaction, _ uuid.UUID, from, to time.Time) (float64, error) {
	if from.After(to) {
		return 0, errors.New("bad range")
	}
	r.incomeCalls++
	if r.incomeCalls == 1 {
		return r.incomeNow, nil
	}
	return r.incomePrev, nil
}
func (r *fakeReader) CountExpiringBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (int, error) {
	return r.expiringWeek, nil
}
func (r *fakeReader) CountExpiredRecoverable(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time, _ int) (int, error) {
	return r.recoverable, nil
}
func (r *fakeReader) TodayCashByMethod(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) (map[string]float64, error) {
	return r.todayCash, nil
}
func (r *fakeReader) IncomeDailySeries(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) ([]reports.DailyIncome, error) {
	return r.series, nil
}
func (r *fakeReader) ListExpiringSoon(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time, _ int) ([]reports.MemberExpiringRow, error) {
	return r.expiringSoon, nil
}
func (r *fakeReader) ListExpiredRecoverable(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time, _, _ int) ([]reports.MemberExpiredRow, error) {
	return r.expiredRecoverable, nil
}
func (r *fakeReader) ListInactiveInvoluntary(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time, _ int) ([]reports.MemberInactiveRow, error) {
	return r.inactiveInvol, nil
}
func (r *fakeReader) ListLowStock(_ sharedDomain.Transaction, _ uuid.UUID) ([]reports.ProductLowStockRow, error) {
	return r.lowStock, nil
}
func (r *fakeReader) ListPendingBalances(_ sharedDomain.Transaction, _ uuid.UUID) ([]reports.PendingBalanceRow, error) {
	return r.pendingBalances, nil
}
func (r *fakeReader) ListBirthdaysOn(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) ([]reports.MemberBirthdayRow, error) {
	return r.birthdays, nil
}
func (r *fakeReader) ListMembersForExport(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) ([]reports.MemberExportRow, error) {
	return r.exportMembers, nil
}
func (r *fakeReader) ListPaymentsForExport(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) ([]reports.PaymentExportRow, error) {
	return r.exportPayments, nil
}
func (r *fakeReader) ListSalesForExport(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) ([]reports.SaleExportRow, error) {
	return r.exportSales, nil
}

// fakeUoW returns a no-op transaction. Reports use cases never actually
// call any methods on Transaction (they pass it straight to the reader).
type fakeUoW struct{}

type fakeTx struct{}

func (fakeTx) Execute(fn func(sharedDomain.Transaction) error) error { return fn(fakeTx{}) }

func (fakeUoW) Begin(context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Commit(sharedDomain.Transaction) error                   { return nil }
func (fakeUoW) Rollback(sharedDomain.Transaction) error                 { return nil }
func (fakeUoW) Query(context.Context) (sharedDomain.Transaction, error) { return fakeTx{}, nil }
func (fakeUoW) Command(ctx context.Context, fn func(sharedDomain.Transaction) error) error {
	return fn(fakeTx{})
}

func TestDashboard_ComposesKPIs(t *testing.T) {
	reader := &fakeReader{
		activeNow:    100,
		activePrev:   80,
		incomeNow:    50000,
		incomePrev:   40000,
		expiringWeek: 12,
		recoverable:  9,
		todayCash:    map[string]float64{"cash": 1500, "card": 800},
		series: []reports.DailyIncome{
			{Date: time.Now().UTC(), Total: 500},
		},
	}
	uc := reports.NewDashboard(reader, fakeUoW{}, 0)

	out, err := uc.Execute(context.Background(), reports.DashboardInput{GymID: uuid.New()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.ActiveMembers.Current != 100 || out.ActiveMembers.Previous != 80 {
		t.Errorf("active KPI = %+v, want 100/80", out.ActiveMembers)
	}
	if out.ActiveMembers.DeltaPct == nil || *out.ActiveMembers.DeltaPct != 25.0 {
		t.Errorf("active delta_pct = %v, want 25", out.ActiveMembers.DeltaPct)
	}
	if out.IncomeMonth.Current != 50000 || out.IncomeMonth.Previous != 40000 {
		t.Errorf("income KPI = %+v", out.IncomeMonth)
	}
	if out.ExpiringThisWeek != 12 {
		t.Errorf("expiring = %d", out.ExpiringThisWeek)
	}
	if out.RecoverableExpired != 9 {
		t.Errorf("recoverable = %d", out.RecoverableExpired)
	}
	if out.TodayCashTotal != 2300 {
		t.Errorf("today cash total = %.2f, want 2300", out.TodayCashTotal)
	}
	if got := out.TodayCash["cash"]; got != 1500 {
		t.Errorf("cash bucket = %.2f, want 1500", got)
	}
}

func TestDashboard_PreviousZeroLeavesDeltaNil(t *testing.T) {
	reader := &fakeReader{activeNow: 5, activePrev: 0}
	uc := reports.NewDashboard(reader, fakeUoW{}, 0)
	out, err := uc.Execute(context.Background(), reports.DashboardInput{GymID: uuid.New()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.ActiveMembers.DeltaPct != nil {
		t.Errorf("expected nil delta_pct when previous is zero")
	}
}

func TestDashboard_CacheServesRepeatCalls(t *testing.T) {
	reader := &fakeReader{activeNow: 7}
	// Use a long TTL so the second call must hit the cache.
	uc := reports.NewDashboard(reader, fakeUoW{}, 60*time.Second)
	gymID := uuid.New()

	if _, err := uc.Execute(context.Background(), reports.DashboardInput{GymID: gymID}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := uc.Execute(context.Background(), reports.DashboardInput{GymID: gymID}); err != nil {
		t.Fatalf("second: %v", err)
	}
	// The reader should have been hit once for active members on the first
	// call and not again on the second.
	if reader.activeCalls != 2 {
		t.Errorf("expected 2 active-count calls (now+prev) on the first run only, got %d", reader.activeCalls)
	}
	uc.InvalidateCache(gymID)
	if _, err := uc.Execute(context.Background(), reports.DashboardInput{GymID: gymID}); err != nil {
		t.Fatalf("after invalidate: %v", err)
	}
	if reader.activeCalls != 4 {
		t.Errorf("expected 4 calls after invalidate, got %d", reader.activeCalls)
	}
}

func TestAttentionRequired_ComposesLists(t *testing.T) {
	reader := &fakeReader{
		expiringSoon: []reports.MemberExpiringRow{
			{MemberID: uuid.New(), FullName: "A", Phone: "1", DaysLeft: 2},
		},
		expiredRecoverable: []reports.MemberExpiredRow{
			{MemberID: uuid.New(), FullName: "B", Phone: "2", DaysOverdue: 5},
		},
		inactiveInvol: []reports.MemberInactiveRow{
			{MemberID: uuid.New(), FullName: "C", Phone: "3", DaysAbsent: 30},
		},
		lowStock: []reports.ProductLowStockRow{
			{ProductID: uuid.New(), Name: "Agua", Stock: 1, StockMinimum: 5},
		},
		pendingBalances: []reports.PendingBalanceRow{
			{MemberID: uuid.New(), FullName: "D", Phone: "4", BalancePending: 250},
		},
		birthdays: []reports.MemberBirthdayRow{
			{MemberID: uuid.New(), FullName: "E", Phone: "5"},
		},
	}
	uc := reports.NewAttentionRequired(reader, fakeUoW{})
	out, err := uc.Execute(context.Background(), reports.AttentionRequiredInput{GymID: uuid.New()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(out.ExpiringSoon) != 1 || len(out.RecoverableExpired) != 1 ||
		len(out.InactiveInvoluntary) != 1 || len(out.LowStock) != 1 ||
		len(out.PendingBalances) != 1 || len(out.BirthdaysToday) != 1 {
		t.Errorf("attention output missing entries: %+v", out)
	}
}
