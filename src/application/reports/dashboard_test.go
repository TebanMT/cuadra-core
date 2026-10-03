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
	cashClosed            float64
	cashCounted           reports.CashCountSummary
	expiringWeek          int
	recoverable           int
	todayCash             map[string]float64
	series                []reports.DailyIncome

	expiringSoon       []reports.MemberExpiringRow
	expiredRecoverable []reports.MemberExpiredRow
	inactiveInvol      []reports.MemberInactiveRow
	lowStock           []reports.ProductLowStockRow
	pendingBalances    []reports.PendingBalanceRow
	birthdayQueries    int
	birthdays          []reports.MemberBirthdayRow

	exportMembers  []reports.MemberExportRow
	exportPayments []reports.PaymentExportRow
	exportSales    []reports.SaleExportRow

	recentPayments []reports.RecentPaymentRow

	newMembersCount int
	checkinsCount   int
	refundsAmount   float64
	incomeByMethod  map[string]float64
	topMembers      []reports.TopMemberRow
	checkinsByDay   []reports.DailyCount

	realizedNow, realizedPrev reports.RealizedProductProfit

	inventoryCost      float64
	inventoryCostRows  []reports.InventoryCostRow
	inventoryListLimit int

	generalExpenses  float64
	expenseRows      []reports.ExpenseRow
	expenseListLimit int

	expensesDaily      []reports.DailyAmount
	expensesByCategory map[string]float64
	topProducts        []reports.TopProductRow
	criticalStock      reports.CriticalStockCounts

	productSalesNow, productSalesPrev reports.ProductSalesTotals
	productSalesCalls                 int

	genderComposition reports.GenderCompositionRow
	genderByHour      []reports.AttendanceByGenderHourRow

	activeCalls   int
	incomeCalls   int
	incomeErr     error
	realizedCalls int
}

type canonicalDashboardReader struct {
	*fakeReader
	current  reports.CanonicalFinancialSnapshot
	previous reports.CanonicalFinancialSnapshot
	calls    int
}

func (r *canonicalDashboardReader) CanonicalFinancialBetween(
	_ sharedDomain.Transaction,
	_ uuid.UUID,
	_ string,
	_, _ time.Time,
) (reports.CanonicalFinancialSnapshot, error) {
	r.calls++
	if r.calls%2 == 1 {
		return r.current, nil
	}
	return r.previous, nil
}

func (r *fakeReader) SumOtherIncomeBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (float64, error) {
	return 0, nil
}

func (r *fakeReader) SumCashClosedBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (float64, error) {
	return r.cashClosed, nil
}

func (r *fakeReader) SumCashCountedBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (reports.CashCountSummary, error) {
	return r.cashCounted, nil
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
	if r.incomeErr != nil {
		return 0, r.incomeErr
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
	r.birthdayQueries++
	return r.birthdays, nil
}
func (r *fakeReader) ListMembersForExport(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) ([]reports.MemberExportRow, error) {
	return r.exportMembers, nil
}
func (r *fakeReader) ListPaymentsForExport(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) ([]reports.PaymentExportRow, error) {
	return r.exportPayments, nil
}
func (r *fakeReader) ListSalesForExport(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _, _ time.Time) ([]reports.SaleExportRow, error) {
	return r.exportSales, nil
}
func (r *fakeReader) CountNewMembersBetween(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _, _ time.Time) (int, error) {
	return r.newMembersCount, nil
}
func (r *fakeReader) CountCheckinsBetween(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _, _ time.Time) (int, error) {
	return r.checkinsCount, nil
}
func (r *fakeReader) SumRefundsBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (float64, error) {
	return r.refundsAmount, nil
}
func (r *fakeReader) IncomeByMethodBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (map[string]float64, error) {
	return r.incomeByMethod, nil
}
func (r *fakeReader) IncomeByMembershipTypeBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (map[string]float64, error) {
	return map[string]float64{}, nil
}
func (r *fakeReader) ActiveMembersByType(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) (map[string]int, error) {
	return map[string]int{}, nil
}
func (r *fakeReader) TopMembersBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time, _ int) ([]reports.TopMemberRow, error) {
	return r.topMembers, nil
}
func (r *fakeReader) CheckinsDailySeries(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _, _ time.Time) ([]reports.DailyCount, error) {
	return r.checkinsByDay, nil
}
func (r *fakeReader) ListRecentPayments(_ sharedDomain.Transaction, _ uuid.UUID, _ int) ([]reports.RecentPaymentRow, error) {
	return r.recentPayments, nil
}
func (r *fakeReader) SumInventoryCostBetween(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _, _ time.Time) (float64, error) {
	return r.inventoryCost, nil
}
func (r *fakeReader) RealizedProductProfitBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (reports.RealizedProductProfit, error) {
	r.realizedCalls++
	if r.realizedCalls == 1 {
		return r.realizedNow, nil
	}
	return r.realizedPrev, nil
}
func (r *fakeReader) ListInventoryCostsBetween(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _, _ time.Time, limit int) ([]reports.InventoryCostRow, error) {
	r.inventoryListLimit = limit
	return r.inventoryCostRows, nil
}
func (r *fakeReader) SumExpensesBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (float64, error) {
	return r.generalExpenses, nil
}
func (r *fakeReader) ListExpensesBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time, limit int) ([]reports.ExpenseRow, error) {
	r.expenseListLimit = limit
	return r.expenseRows, nil
}
func (r *fakeReader) ExpensesDailySeries(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _, _ time.Time) ([]reports.DailyAmount, error) {
	return r.expensesDaily, nil
}
func (r *fakeReader) ExpensesByCategoryBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (map[string]float64, error) {
	return r.expensesByCategory, nil
}
func (r *fakeReader) TopProductsBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time, _ int) ([]reports.TopProductRow, error) {
	return r.topProducts, nil
}
func (r *fakeReader) SumProductSalesBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) (reports.ProductSalesTotals, error) {
	r.productSalesCalls++
	if r.productSalesCalls == 1 {
		return r.productSalesNow, nil
	}
	return r.productSalesPrev, nil
}
func (r *fakeReader) CountCriticalStock(_ sharedDomain.Transaction, _ uuid.UUID) (reports.CriticalStockCounts, error) {
	return r.criticalStock, nil
}
func (r *fakeReader) GenderComposition(_ sharedDomain.Transaction, _ uuid.UUID, _ time.Time) (reports.GenderCompositionRow, error) {
	return r.genderComposition, nil
}
func (r *fakeReader) AttendanceByGenderHour(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _ int, _ time.Time) ([]reports.AttendanceByGenderHourRow, error) {
	return r.genderByHour, nil
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
		activeNow:       100,
		activePrev:      80,
		incomeNow:       50000,
		incomePrev:      40000,
		expiringWeek:    12,
		recoverable:     9,
		inventoryCost:   300,
		generalExpenses: 700,
		refundsAmount:   250,
		todayCash:       map[string]float64{"cash": 1500, "card": 800},
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
	// Egresos del mes = mercancía + gastos generales + devoluciones
	//                 = 300 + 700 + 250 = 1250. Las devoluciones son dinero
	// que salió del cajón — si no entran aquí, el dashboard las ignora por
	// completo (IncomeMonth es bruto y no las resta).
	if out.ExpensesMonth.Current != 1250 {
		t.Errorf("expenses_month.current = %.2f, want 1250", out.ExpensesMonth.Current)
	}
	if out.RefundsMonth.Current != 250 {
		t.Errorf("refunds_month.current = %.2f, want 250", out.RefundsMonth.Current)
	}
}

func TestDashboard_RealizedProfitKPIAndCoverage(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	reader := &fakeReader{
		realizedNow:  reports.RealizedProductProfit{Revenue: 1000, COGS: 600, ItemsTotal: 8, ItemsWithCost: 5},
		realizedPrev: reports.RealizedProductProfit{Revenue: 800, COGS: 500},
	}
	g := sampleGym()
	g.SubscriptionPlan = "plus_monthly"
	uc := reports.NewDashboard(reader, fakeUoW{}, 0).WithGyms(&fakeGymRepo{gym: g})
	out, err := uc.Execute(context.Background(), reports.DashboardInput{GymID: uuid.New()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// realized = revenue − COGS: actual 400, previo 300.
	if out.RealizedProfitMonth.Current != 400 || out.RealizedProfitMonth.Previous != 300 {
		t.Errorf("realized KPI = %+v, want 400/300", out.RealizedProfitMonth)
	}
	// Cobertura del rango actual (la del previo no se expone).
	if out.RealizedProfitCoverage.ItemsTotal != 8 || out.RealizedProfitCoverage.ItemsWithCost != 5 {
		t.Errorf("cobertura = %+v, want 5/8", out.RealizedProfitCoverage)
	}
	// Margen = utilidad / ingreso = (1000−600)/1000 = 40%.
	if out.RealizedProfitMarginPct == nil || *out.RealizedProfitMarginPct != 40 {
		t.Errorf("margen = %v, want 40", out.RealizedProfitMarginPct)
	}
}

func TestDashboard_RealizedMarginNilWhenNoProductSales(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	// Sin ventas de productos (Revenue 0) → margen nil (no se muestra chip).
	reader := &fakeReader{realizedNow: reports.RealizedProductProfit{}}
	g := sampleGym()
	g.SubscriptionPlan = "plus_monthly"
	uc := reports.NewDashboard(reader, fakeUoW{}, 0).WithGyms(&fakeGymRepo{gym: g})
	out, err := uc.Execute(context.Background(), reports.DashboardInput{GymID: uuid.New()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.RealizedProfitMarginPct != nil {
		t.Errorf("margen = %v, want nil (sin ventas)", *out.RealizedProfitMarginPct)
	}
}

func TestDashboard_StandardOmitsProductProfitWithoutQueryingIt(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	reader := &fakeReader{
		realizedNow:  reports.RealizedProductProfit{Revenue: 1000, COGS: 600, ItemsTotal: 8, ItemsWithCost: 5},
		realizedPrev: reports.RealizedProductProfit{Revenue: 800, COGS: 500},
	}
	g := sampleGym()
	g.SubscriptionPlan = "standard_monthly"
	out, err := reports.NewDashboard(reader, fakeUoW{}, 0).WithGyms(&fakeGymRepo{gym: g}).Execute(
		context.Background(), reports.DashboardInput{GymID: uuid.New()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.RealizedProfitMonth != nil || out.RealizedProfitCoverage != nil || out.RealizedProfitMarginPct != nil {
		t.Fatalf("standard leaked product profit: %+v / %+v / %v", out.RealizedProfitMonth, out.RealizedProfitCoverage, out.RealizedProfitMarginPct)
	}
	if reader.realizedCalls != 0 {
		t.Fatalf("standard executed %d premium profit queries, want 0", reader.realizedCalls)
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

func TestDashboard_AlwaysReadsCommittedStateInsteadOfServingStaleCache(t *testing.T) {
	reader := &canonicalDashboardReader{
		fakeReader: &fakeReader{activeNow: 7},
		current:    reports.CanonicalFinancialSnapshot{MembershipIncome: 100},
	}
	// A long legacy TTL must not change the fresh-read guarantee.
	uc := reports.NewDashboard(reader, fakeUoW{}, 60*time.Second)
	gymID := uuid.New()

	first, err := uc.Execute(context.Background(), reports.DashboardInput{GymID: gymID})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	reader.current.MembershipIncome = 125 // committed mutation between requests
	second, err := uc.Execute(context.Background(), reports.DashboardInput{GymID: gymID})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.IncomeMonth.Current != 100 || second.IncomeMonth.Current != 125 {
		t.Fatalf("income first/second = %.2f/%.2f, want 100/125", first.IncomeMonth.Current, second.IncomeMonth.Current)
	}
	if reader.calls != 4 { // current + previous for each request
		t.Fatalf("canonical reads = %d, want 4", reader.calls)
	}
	// Compatibility hook is deliberately harmless now.
	uc.InvalidateCache(gymID)
}

func TestDashboard_CanonicalEquationMatchesGoldenModel(t *testing.T) {
	reader := &canonicalDashboardReader{
		fakeReader: &fakeReader{},
		current: reports.CanonicalFinancialSnapshot{
			MembershipIncome:   400,
			ProductIncome:      15,
			OperatingExpenses:  1500,
			InventoryPurchases: 50,
		},
	}
	out, err := reports.NewDashboard(reader, fakeUoW{}, 0).Execute(context.Background(), reports.DashboardInput{GymID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if out.IncomeMonth.Current != 415 || out.ExpensesMonth.Current != 1550 || out.PeriodResultMonth.Current != -1135 {
		t.Fatalf("income/outflows/result = %.2f/%.2f/%.2f, want 415/1550/-1135",
			out.IncomeMonth.Current, out.ExpensesMonth.Current, out.PeriodResultMonth.Current)
	}
	if out.MembershipIncomeMonth.Current != 400 || out.ProductIncomeMonth.Current != 15 || out.OtherIncomeMonth.Current != 0 {
		t.Fatalf("income breakdown = membership %.2f, product %.2f, other %.2f",
			out.MembershipIncomeMonth.Current, out.ProductIncomeMonth.Current, out.OtherIncomeMonth.Current)
	}
	if out.OperatingExpensesMonth.Current != 1500 || out.InventoryPurchasesMonth.Current != 50 || out.RefundsMonth.Current != 0 {
		t.Fatalf("outflow breakdown = expenses %.2f, purchases %.2f, refunds %.2f",
			out.OperatingExpensesMonth.Current, out.InventoryPurchasesMonth.Current, out.RefundsMonth.Current)
	}
}

func TestDashboard_ComposesEveryReadInsideOneSnapshot(t *testing.T) {
	uow := &snapshotSpyUoW{}
	_, err := reports.NewDashboard(&fakeReader{}, uow, 60*time.Second).Execute(context.Background(), reports.DashboardInput{GymID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if uow.snapshots != 1 || uow.queries != 0 {
		t.Fatalf("snapshots=%d queries=%d, want one snapshot and no loose query", uow.snapshots, uow.queries)
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
		len(out.PendingBalances) != 1 || len(out.BirthdaysToday) != 0 {
		t.Errorf("attention output missing entries: %+v", out)
	}
}

func TestBirthdayNoticesRetired(t *testing.T) {
	reader := &fakeReader{birthdays: []reports.MemberBirthdayRow{{MemberID: uuid.New(), FullName: "Birthday fixture"}}}
	gym := uuid.New()
	attention, err := reports.NewAttentionRequired(reader, fakeUoW{}).Execute(context.Background(), reports.AttentionRequiredInput{GymID: gym})
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := reports.NewDashboard(reader, fakeUoW{}, 0).Execute(context.Background(), reports.DashboardInput{GymID: gym})
	if err != nil {
		t.Fatal(err)
	}
	if reader.birthdayQueries != 0 || len(attention.BirthdaysToday) != 0 || dashboard.AttentionSummary.BirthdaysToday != 0 {
		t.Fatalf("retired birthday notices were queried or returned: queries=%d", reader.birthdayQueries)
	}
}
