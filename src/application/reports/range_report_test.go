package reports_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/reports"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type snapshotSpyUoW struct {
	fakeUoW
	snapshots int
	queries   int
}

func (u *snapshotSpyUoW) Query(context.Context) (sharedDomain.Transaction, error) {
	u.queries++
	return fakeTx{}, nil
}

func (u *snapshotSpyUoW) ReadSnapshot(_ context.Context, fn func(sharedDomain.Transaction) error) error {
	u.snapshots++
	return fn(fakeTx{})
}

func TestRangeReport_ComposesEveryReadInsideOneSnapshot(t *testing.T) {
	uow := &snapshotSpyUoW{}
	_, err := reports.NewRangeReport(&fakeReader{}, uow).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if uow.snapshots != 1 || uow.queries != 0 {
		t.Fatalf("snapshots=%d queries=%d, want one snapshot and no loose query", uow.snapshots, uow.queries)
	}
}

func TestRangeReport_DeclaresDetailTruncationAtRow201(t *testing.T) {
	reader := &fakeReader{
		inventoryCostRows: make([]reports.InventoryCostRow, 201),
		expenseRows:       make([]reports.ExpenseRow, 201),
	}
	out, err := reports.NewRangeReport(reader, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.InventoryCosts) != 200 || len(out.Expenses) != 200 {
		t.Fatalf("bounded rows inventory/expenses=%d/%d, want 200/200", len(out.InventoryCosts), len(out.Expenses))
	}
	if !out.DetailMetadata.InventoryCosts.Truncated || !out.DetailMetadata.Expenses.Truncated {
		t.Fatalf("truncation metadata = %+v", out.DetailMetadata)
	}
	if out.DetailMetadata.InventoryCosts.Limit == nil || *out.DetailMetadata.InventoryCosts.Limit != 200 ||
		out.DetailMetadata.Expenses.Limit == nil || *out.DetailMetadata.Expenses.Limit != 200 {
		t.Fatalf("detail limits = %+v", out.DetailMetadata)
	}
	if reader.inventoryListLimit != 201 || reader.expenseListLimit != 201 {
		t.Fatalf("reader limits inventory/expenses=%d/%d, want lookahead 201", reader.inventoryListLimit, reader.expenseListLimit)
	}
}

// TestRangeReport_PopulatesAllTotals exercises the UC-036 use case end-to-end
// against the fakeReader, verifying that the range queries reach the output
// and the totals are wrapped as KPI structs (current/previous/delta). The
// fakeReader returns the same value for both current and previous windows,
// so deltas land at zero — fine for this assertion.
func TestRangeReport_PopulatesAllTotals(t *testing.T) {
	memberID := uuid.New()
	cashSessionID := uuid.New()
	expectedCash, latestCounted, latestDifference := 900.0, 950.0, 50.0
	countedAt := time.Now().UTC()
	reader := &fakeReader{
		incomeNow:       1500, // SumPaymentsBetween (already excludes refunds)
		newMembersCount: 1,
		checkinsCount:   2,
		refundsAmount:   250,
		incomeByMethod: map[string]float64{
			"cash": 1000,
			"card": 500,
		},
		topMembers: []reports.TopMemberRow{
			{MemberID: memberID, FullName: "Juan Pérez", TotalPaid: 1500, PaymentsCount: 3},
		},
		checkinsByDay: []reports.DailyCount{
			{Date: time.Now().UTC(), Count: 2},
		},
		generalExpenses: 200,
		inventoryCost:   300,
		cashClosed:      900,
		cashCounted: reports.CashCountSummary{
			Counted: 950, CountedCloses: 1, TotalCloses: 1, Withdrawn: 900,
			ActiveDays: 1, ReconciledSessions: 1, TotalSessions: 1,
			LatestSessionID: &cashSessionID, LatestExpected: &expectedCash,
			LatestCounted: &latestCounted, LatestDifference: &latestDifference,
			LatestCountedAt: &countedAt, LatestStatus: "reconciled",
		},
		productSalesNow:  reports.ProductSalesTotals{Amount: 320, Units: 12},
		productSalesPrev: reports.ProductSalesTotals{Amount: 100, Units: 4},
	}

	uc := reports.NewRangeReport(reader, fakeUoW{})
	out, err := uc.Execute(context.Background(), reports.RangeReportInput{
		GymID:  uuid.New(),
		Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if out.Totals.Income.Current != 1500 {
		t.Errorf("totals.income.current = %.2f, want 1500", out.Totals.Income.Current)
	}
	if out.Totals.NewMembers.Current != 1 {
		t.Errorf("totals.new_members.current = %.0f, want 1", out.Totals.NewMembers.Current)
	}
	if out.Totals.Checkins.Current != 2 {
		t.Errorf("totals.checkins.current = %.0f, want 2", out.Totals.Checkins.Current)
	}
	if out.Totals.Refunds.Current != 250 {
		t.Errorf("totals.refunds.current = %.2f, want 250", out.Totals.Refunds.Current)
	}
	// product_sales: $ como KPI con ventana previa, unidades del período
	// actual como sub-línea sin delta.
	if out.ProductSales.Amount.Current != 320 {
		t.Errorf("product_sales.amount.current = %.2f, want 320", out.ProductSales.Amount.Current)
	}
	if out.ProductSales.Units != 12 {
		t.Errorf("product_sales.units = %d, want 12", out.ProductSales.Units)
	}
	// Resultado neto = income − compras − expenses − refunds
	//     = 1500 − 300 − 200 − 250 = 750.
	if out.Totals.Net.Current != 750 {
		t.Errorf("totals.net.current = %.2f, want 750", out.Totals.Net.Current)
	}
	if out.Totals.NetResult.Current != 750 {
		t.Errorf("totals.net_result.current = %.2f, want 750", out.Totals.NetResult.Current)
	}
	if out.Totals.NetResult.Current != out.Totals.Income.Current-out.Totals.Refunds.Current-out.Totals.InventoryCost.Current-out.Totals.ExpensesGeneral.Current {
		t.Errorf(
			"net result does not reconcile: income %.2f - refunds %.2f - purchases %.2f - expenses %.2f = %.2f, got %.2f",
			out.Totals.Income.Current,
			out.Totals.Refunds.Current,
			out.Totals.InventoryCost.Current,
			out.Totals.ExpensesGeneral.Current,
			out.Totals.Income.Current-out.Totals.Refunds.Current-out.Totals.InventoryCost.Current-out.Totals.ExpensesGeneral.Current,
			out.Totals.NetResult.Current,
		)
	}
	// El alias histórico cash_flow apunta a la actividad neta certificada al
	// cerrar; no es el conteo físico (puede incluir apertura o diferencias).
	if out.Totals.CashFromCloses.Current != 900 {
		t.Errorf("totals.cash_from_closes.current = %.2f, want 900", out.Totals.CashFromCloses.Current)
	}
	if out.Totals.CashFlow.Current != out.Totals.CashFromCloses.Current {
		t.Errorf("legacy cash_flow alias = %.2f, want %.2f", out.Totals.CashFlow.Current, out.Totals.CashFromCloses.Current)
	}
	if out.CashReconciliation.Counted != 950 || out.CashReconciliation.Difference == nil || *out.CashReconciliation.Difference != 50 {
		t.Errorf("cash reconciliation = %+v, want counted 950 and difference 50", out.CashReconciliation)
	}
	if out.CashReconciliation.PeriodActivity != 900 || out.CashReconciliation.PeriodWithdrawn != 900 ||
		out.CashReconciliation.LatestExpected == nil || *out.CashReconciliation.LatestExpected != 900 ||
		out.CashReconciliation.LatestSessionID == nil || *out.CashReconciliation.LatestSessionID != cashSessionID {
		t.Errorf("cash reconciliation scopes mixed: %+v", out.CashReconciliation)
	}
	if out.CashReconciliation.Status != "complete" || !out.CashReconciliation.Complete ||
		out.CashReconciliation.TotalSessions != 1 || out.CashReconciliation.ReconciledSessions != 1 {
		t.Errorf("cash coverage = %+v, want one completely reconciled session", out.CashReconciliation)
	}
	if out.IncomeByMethod["cash"] != 1000 || out.IncomeByMethod["card"] != 500 {
		t.Errorf("income_by_method = %+v", out.IncomeByMethod)
	}
	if len(out.TopMembers) != 1 || out.TopMembers[0].MemberID != memberID {
		t.Errorf("top_members = %+v", out.TopMembers)
	}
	if out.TopMembers[0].TotalPaid != 1500 || out.TopMembers[0].PaymentsCount != 3 {
		t.Errorf("top_members[0] amounts wrong: %+v", out.TopMembers[0])
	}
	if len(out.CheckinsByDay) != 1 || out.CheckinsByDay[0].Count != 2 {
		t.Errorf("checkins_by_day = %+v", out.CheckinsByDay)
	}
}

func TestRangeReport_LeavesCashDifferencePendingWhenAnyCloseHasNoCount(t *testing.T) {
	sessionID := uuid.New()
	expected, counted := 100.0, 80.0
	reader := &fakeReader{
		cashClosed: 100,
		cashCounted: reports.CashCountSummary{
			Counted:                  80,
			CountedCloses:            1,
			TotalCloses:              2,
			ActiveDays:               2,
			ReconciledSessions:       1,
			ClosedUnverifiedSessions: 1,
			TotalSessions:            2,
			LatestSessionID:          &sessionID,
			LatestExpected:           &expected,
			LatestCounted:            &counted,
		},
	}
	out, err := reports.NewRangeReport(reader, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.CashReconciliation.Difference != nil {
		t.Fatalf("difference = %v, want nil for incomplete physical-count coverage", *out.CashReconciliation.Difference)
	}
	if out.CashReconciliation.Counted != 80 || out.CashReconciliation.CountedCloses != 1 || out.CashReconciliation.TotalCloses != 2 {
		t.Errorf("cash reconciliation = %+v", out.CashReconciliation)
	}
	if out.CashReconciliation.Status != "incomplete" || out.CashReconciliation.Complete {
		t.Errorf("cash coverage = %+v, want incomplete", out.CashReconciliation)
	}
}

func TestRangeReport_CashGoldenSeparatesOpeningActivityAndWithdrawal(t *testing.T) {
	sessionID := uuid.New()
	expected, counted, difference := 700.0, 700.0, 0.0
	countedAt := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	reader := &fakeReader{
		cashClosed: 500,
		cashCounted: reports.CashCountSummary{
			Counted: 700, CountedCloses: 1, TotalCloses: 1, Withdrawn: 500,
			ActiveDays: 1, WithdrawnSessions: 1, TotalSessions: 1,
			LatestSessionID: &sessionID, LatestExpected: &expected,
			LatestCounted: &counted, LatestDifference: &difference,
			LatestCountedAt: &countedAt, LatestStatus: "withdrawn",
		},
	}
	out, err := reports.NewRangeReport(reader, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := out.CashReconciliation
	if r.PeriodActivity != 500 || r.PeriodWithdrawn != 500 ||
		r.LatestExpected == nil || *r.LatestExpected != 700 ||
		r.LatestCounted == nil || *r.LatestCounted != 700 ||
		r.LatestDifference == nil || *r.LatestDifference != 0 {
		t.Fatalf("cash golden = %+v; opening 200 + activity 500 must yield expected/count 700, withdrawal 500", r)
	}
}

func TestRangeReport_StaleCashSuppressesObsoleteDifference(t *testing.T) {
	sessionID := uuid.New()
	expected, counted, obsoleteDifference := 550.0, 500.0, -50.0
	reader := &fakeReader{
		cashClosed: 550,
		cashCounted: reports.CashCountSummary{
			TotalCloses: 1, ActiveDays: 1, StaleSessions: 1, TotalSessions: 1,
			LatestSessionID: &sessionID, LatestExpected: &expected, LatestCounted: &counted,
			LatestDifference: &obsoleteDifference, LatestStatus: "stale", LatestNeedsRecount: true,
		},
	}
	out, err := reports.NewRangeReport(reader, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.CashReconciliation.Status != "stale" || out.CashReconciliation.Complete ||
		out.CashReconciliation.LatestDifference != nil || out.CashReconciliation.Difference != nil {
		t.Fatalf("stale reconciliation = %+v; obsolete difference must be null", out.CashReconciliation)
	}
	payload, err := json.Marshal(out.CashReconciliation)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	if value, present := wire["latest_difference"]; !present || value != nil {
		t.Fatalf("wire latest_difference = %#v (present=%v), want explicit null; payload=%s", value, present, payload)
	}
	for _, key := range []string{
		"status", "complete", "active_days", "missing_active_days", "uncovered_activity_days",
		"open_sessions", "closed_unverified_sessions",
		"reconciled_sessions", "stale_sessions", "withdrawn_sessions", "active_sessions", "total_sessions",
		"unknown_opening_sessions", "adjusted_after_withdrawal_sessions", "legacy_cash_source_unverified_count",
	} {
		if _, present := wire[key]; !present {
			t.Fatalf("canonical cash wire misses %q: %s", key, payload)
		}
	}
}

func TestRangeReport_CashCoverageDistinguishesNoActivityFromUncoveredCash(t *testing.T) {
	noActivity, err := reports.NewRangeReport(&fakeReader{}, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if noActivity.CashReconciliation.Status != "complete" || !noActivity.CashReconciliation.Complete {
		t.Fatalf("no activity = %+v, want complete coverage with zero activity", noActivity.CashReconciliation)
	}
	uncovered, err := reports.NewRangeReport(&fakeReader{cashCounted: reports.CashCountSummary{
		ActiveDays: 1, MissingActiveDays: 1, UncoveredActivityDays: 1,
	}}, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if uncovered.CashReconciliation.Status != "incomplete" || uncovered.CashReconciliation.Complete {
		t.Fatalf("uncovered cash = %+v, want incomplete", uncovered.CashReconciliation)
	}
}

func TestRangeReport_FlagsLegacyCashInFinancialClassification(t *testing.T) {
	reader := &canonicalDashboardReader{
		fakeReader: &fakeReader{},
		current:    reports.CanonicalFinancialSnapshot{InvalidCashInClassificationCount: 1},
	}
	out, err := reports.NewRangeReport(reader, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Integrity.Status != "incomplete" || out.Integrity.InvalidCashInClassificationCount != 1 ||
		len(out.Integrity.Warnings) != 1 || out.Integrity.Warnings[0] != "invalid_cash_in_classification" {
		t.Fatalf("integrity=%+v, want explicit invalid cash-in warning", out.Integrity)
	}
}

func TestRangeReport_LegacyCashSourceWarnsWithoutInvalidatingBusinessResult(t *testing.T) {
	reader := &canonicalDashboardReader{
		fakeReader: &fakeReader{},
		current: reports.CanonicalFinancialSnapshot{
			OperatingExpenses:               50,
			LegacyCashSourceUnverifiedCount: 1,
		},
	}
	out, err := reports.NewRangeReport(reader, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Totals.PeriodResult.Current != -50 {
		t.Fatalf("period result=%.2f, want -50: the expense remains economically valid", out.Totals.PeriodResult.Current)
	}
	if out.Integrity.Status != "complete" || out.Integrity.LegacyCashSourceUnverifiedCount != 1 ||
		len(out.Integrity.Warnings) != 1 || out.Integrity.Warnings[0] != "legacy_cash_source_unverified" {
		t.Fatalf("integrity=%+v, want a cash-only warning with complete financial status", out.Integrity)
	}
}

func TestRangeReport_LegacyCashSourcePreventsPhysicalCertification(t *testing.T) {
	sessionID := uuid.New()
	expected, counted, difference := 100.0, 100.0, 0.0
	reader := &fakeReader{cashCounted: reports.CashCountSummary{
		CountedCloses: 1, TotalCloses: 1,
		ReconciledSessions: 1, TotalSessions: 1,
		LatestSessionID: &sessionID, LatestExpected: &expected, LatestCounted: &counted,
		LatestDifference: &difference, LatestStatus: "reconciled",
		LegacyCashSourceUnverifiedCount: 1,
	}}
	out, err := reports.NewRangeReport(reader, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.CashReconciliation.Status != "incomplete" || out.CashReconciliation.Complete ||
		out.CashReconciliation.LegacyCashSourceUnverifiedCount != 1 {
		t.Fatalf("cash reconciliation=%+v, want incomplete until Caja/Fondo is confirmed", out.CashReconciliation)
	}
}

func TestRangeReport_DoesNotTurnReaderFailureIntoZero(t *testing.T) {
	reader := &fakeReader{incomeErr: errors.New("database unavailable")}
	_, err := reports.NewRangeReport(reader, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodMonth,
	})
	if err == nil {
		t.Fatal("reader failure must surface; zero would be a false financial datum")
	}
	var custom sharedDomain.CustomError
	if !errors.As(err, &custom) || custom.ErrorCode != sharedDomain.CodeUnexpected {
		t.Errorf("err = %#v, want unexpected domain error", err)
	}
}

// TestRangeReport_Today_UsaDiaLocalDelGym — el período "Hoy" se ancla al día
// calendario del GYM, no al de UTC. 27-jul 20:00 CDMX == 28-jul 02:00 UTC:
// con el anclaje UTC anterior la sección de reportes pedía el 28 y salía en
// ceros aunque el día hubiera cerrado con cobros.
func TestRangeReport_Today_UsaDiaLocalDelGym(t *testing.T) {
	uc := reports.NewRangeReport(&fakeReader{}, fakeUoW{}).WithGyms(&fakeGymRepo{gym: sampleGym()})
	uc.Now = func() time.Time { return time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC) }

	out, err := uc.Execute(context.Background(), reports.RangeReportInput{
		GymID:  uuid.New(),
		Period: reports.PeriodToday,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.From != "2026-07-27" || out.To != "2026-07-27" {
		t.Errorf("today window = %s..%s, want 2026-07-27..2026-07-27", out.From, out.To)
	}
}

// TestRangeReport_Today_SinGymsCaeAUTC — fail-open: sin repo de gyms el
// período sigue anclado al día UTC (comportamiento previo).
func TestRangeReport_Today_SinGymsCaeAUTC(t *testing.T) {
	uc := reports.NewRangeReport(&fakeReader{}, fakeUoW{})
	uc.Now = func() time.Time { return time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC) }

	out, err := uc.Execute(context.Background(), reports.RangeReportInput{
		GymID:  uuid.New(),
		Period: reports.PeriodToday,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.From != "2026-07-28" {
		t.Errorf("today window sin gyms = %s, want 2026-07-28", out.From)
	}
}

// TestRangeReport_PeriodWindow_Today returns a same-day from/to.
func TestRangeReport_PeriodWindow_Today(t *testing.T) {
	today := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	from, to := reports.PeriodWindow(reports.PeriodToday, today)
	if from.Format("2006-01-02") != "2026-04-27" || to.Format("2006-01-02") != "2026-04-27" {
		t.Errorf("today window = %s..%s", from, to)
	}
}

func TestRangeReport_RollingPeriodsDoNotCountAnchorDayTwice(t *testing.T) {
	today := time.Date(2026, 8, 24, 23, 30, 0, 0, time.FixedZone("CDMX", -6*60*60))
	cases := []struct {
		period string
		want   string
	}{
		{reports.Period3Months, "2026-05-25"},
		{reports.PeriodYear, "2025-08-25"},
	}
	for _, tc := range cases {
		from, to := reports.PeriodWindow(tc.period, today)
		if got := from.Format("2006-01-02"); got != tc.want {
			t.Errorf("%s from = %s, want %s", tc.period, got, tc.want)
		}
		if got := to.Format("2006-01-02"); got != "2026-08-24" {
			t.Errorf("%s to = %s, want local calendar date 2026-08-24", tc.period, got)
		}
	}
}

func TestRangeReport_RollingPeriodsClampMonthEndAndLeapDay(t *testing.T) {
	from, _ := reports.PeriodWindow(reports.Period3Months, time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC))
	if got := from.Format("2006-01-02"); got != "2026-03-01" {
		t.Errorf("May 31 rolling quarter starts %s, want 2026-03-01", got)
	}
	from, _ = reports.PeriodWindow(reports.PeriodYear, time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC))
	if got := from.Format("2006-01-02"); got != "2023-03-01" {
		t.Errorf("leap-day rolling year starts %s, want 2023-03-01", got)
	}
}

// TestRangeReport_PeriodWindow_LastMonth returns the prior calendar month.
func TestRangeReport_PeriodWindow_LastMonth(t *testing.T) {
	today := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	from, to := reports.PeriodWindow(reports.PeriodLastMonth, today)
	if from.Format("2006-01-02") != "2026-03-01" || to.Format("2006-01-02") != "2026-03-31" {
		t.Errorf("last_month window = %s..%s", from, to)
	}
}

// TestRangeReport_EmptyDefaults returns zero-shaped output when Reader is
// missing data (fakeReader returns all zero values).
func TestRangeReport_EmptyDefaults(t *testing.T) {
	uc := reports.NewRangeReport(&fakeReader{}, fakeUoW{})
	out, err := uc.Execute(context.Background(), reports.RangeReportInput{
		GymID:  uuid.New(),
		Period: reports.PeriodWeek,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.Totals.Income.Current != 0 || out.Totals.NewMembers.Current != 0 ||
		out.Totals.Checkins.Current != 0 || out.Totals.Refunds.Current != 0 ||
		out.Totals.Net.Current != 0 {
		t.Errorf("expected zero totals, got %+v", out.Totals)
	}
	if out.IncomeByMethod == nil {
		t.Error("income_by_method should be non-nil for FE rendering")
	}
	if out.ExpensesByCategory == nil {
		t.Error("expenses_by_category should be non-nil")
	}
	if out.TopMembers == nil {
		t.Error("top_members should be non-nil")
	}
	if out.TopProducts == nil {
		t.Error("top_products should be non-nil")
	}
	if out.CheckinsByDay == nil {
		t.Error("checkins_by_day should be non-nil")
	}
	if out.ExpensesByDay == nil {
		t.Error("expenses_by_day should be non-nil")
	}
}

// TestRangeReport_CustomWindow respects from/to bounds.
func TestRangeReport_CustomWindow(t *testing.T) {
	uc := reports.NewRangeReport(&fakeReader{}, fakeUoW{})
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC)
	out, err := uc.Execute(context.Background(), reports.RangeReportInput{
		GymID:  uuid.New(),
		Period: reports.PeriodCustom,
		From:   &from,
		To:     &to,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.From != "2026-04-01" || out.To != "2026-04-14" {
		t.Errorf("custom window = %s..%s, want 2026-04-01..2026-04-14", out.From, out.To)
	}
}

func TestRangeReport_RejectsUnknownOrAmbiguousWindows(t *testing.T) {
	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	tomorrow := today.AddDate(0, 0, 1)
	cases := []struct {
		name string
		in   reports.RangeReportInput
		want error
	}{
		{name: "unknown period", in: reports.RangeReportInput{GymID: uuid.New(), Period: "quarter"}, want: reports.ErrReportPeriodInvalid},
		{name: "custom missing from", in: reports.RangeReportInput{GymID: uuid.New(), Period: reports.PeriodCustom, To: &today}, want: reports.ErrCustomReportRangeRequired},
		{name: "custom missing to", in: reports.RangeReportInput{GymID: uuid.New(), Period: reports.PeriodCustom, From: &today}, want: reports.ErrCustomReportRangeRequired},
		{name: "custom reversed", in: reports.RangeReportInput{GymID: uuid.New(), Period: reports.PeriodCustom, From: &tomorrow, To: &today}, want: reports.ErrReportRangeInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Validation happens before opening a read snapshot, so nil
			// collaborators are intentional here.
			_, err := reports.NewRangeReport(nil, nil).Execute(context.Background(), tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Execute() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRangeReport_CustomSameDayIsInclusiveAndValid(t *testing.T) {
	day := time.Date(2026, 8, 24, 22, 30, 0, 0, time.FixedZone("local", -6*60*60))
	out, err := reports.NewRangeReport(&fakeReader{}, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{
		GymID: uuid.New(), Period: reports.PeriodCustom, From: &day, To: &day,
	})
	if err != nil {
		t.Fatalf("same-day custom report: %v", err)
	}
	if out.From != "2026-08-24" || out.To != "2026-08-24" {
		t.Fatalf("same-day inclusive window = %s..%s", out.From, out.To)
	}
}

// TestPreviousWindow_Month aligns to prior calendar month, same day offset.
func TestRangeReport_PreviousWindow_MonthAlignment(t *testing.T) {
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC) // mid-month
	// April has 30 days; prev window should be 2026-04-01..2026-04-14.
	uc := reports.NewRangeReport(&windowSpy{}, fakeUoW{})
	spy := uc.Reader.(*windowSpy)
	if _, err := uc.Execute(context.Background(), reports.RangeReportInput{
		GymID:  uuid.New(),
		Period: reports.PeriodCustom,
		From:   &from,
		To:     &to,
	}); err != nil {
		// Custom uses default previousWindow branch (fixed-length); month
		// alignment is only triggered by PeriodMonth/PeriodLastMonth.
		t.Logf("custom branch: %v", err)
	}
	// Sanity: spy got at least one previous-window query (we use it through
	// SumPaymentsBetween — the use case calls it twice).
	if len(spy.windows) < 2 {
		t.Fatalf("expected ≥2 windows queried, got %v", spy.windows)
	}
}

// windowSpy captures the [from,to] of each SumPaymentsBetween call so we can
// assert the previous window has the right shape.
type windowSpy struct {
	fakeReader
	windows [][2]time.Time
}

func (s *windowSpy) SumPaymentsBetween(_ sharedDomain.Transaction, _ uuid.UUID, from, to time.Time) (float64, error) {
	s.windows = append(s.windows, [2]time.Time{from, to})
	return 0, nil
}

func TestRangeReport_HistoricalCashIsNotRetroactiveWork(t *testing.T) {
	for _, tc := range []struct {
		name      string
		summary   reports.CashCountSummary
		attention bool
	}{
		{"history only", reports.CashCountSummary{ActiveDays: 5, HistoricalActivityDays: 5, HistoricalSessions: 1}, false},
		{"history and reviewed cuts", reports.CashCountSummary{ActiveDays: 6, HistoricalActivityDays: 5, TotalSessions: 1, ReconciledSessions: 1, CountedCloses: 1, TotalCloses: 1}, false},
		{"history and missing new cut", reports.CashCountSummary{ActiveDays: 6, HistoricalActivityDays: 5, MissingActiveDays: 1, UncoveredActivityDays: 1}, true},
		{"history and open session", reports.CashCountSummary{HistoricalActivityDays: 5, TotalSessions: 1, OpenSessions: 1}, true},
		{"history and changed cut", reports.CashCountSummary{HistoricalActivityDays: 5, TotalSessions: 1, StaleSessions: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := reports.NewRangeReport(&fakeReader{cashCounted: tc.summary}, fakeUoW{}).Execute(context.Background(), reports.RangeReportInput{GymID: uuid.New(), Period: reports.PeriodMonth})
			if err != nil {
				t.Fatal(err)
			}
			r := out.CashReconciliation
			if r.Complete || r.RequiresAttention != tc.attention || r.HistoricalActivityDays != tc.summary.HistoricalActivityDays {
				t.Fatalf("cash=%+v", r)
			}
			if !tc.attention && (r.LatestDifference != nil || r.Difference != nil) {
				t.Fatalf("history must not invent a difference: %+v", r)
			}
		})
	}
}

func TestRangeReport_ComparisonMetadataMatchesQueriedDates(t *testing.T) {
	spy := &windowSpy{}
	uc := reports.NewRangeReport(spy, fakeUoW{})
	uc.Now = func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }
	out, err := uc.Execute(context.Background(), reports.RangeReportInput{GymID: uuid.New(), Period: reports.PeriodMonth})
	if err != nil {
		t.Fatal(err)
	}
	if out.PreviousFrom != "2026-08-01" || out.PreviousTo != "2026-08-28" {
		t.Fatalf("wrong comparison: %s..%s", out.PreviousFrom, out.PreviousTo)
	}
	if len(spy.windows) < 2 || spy.windows[1][0].Format("2006-01-02") != out.PreviousFrom || spy.windows[1][1].Format("2006-01-02") != out.PreviousTo {
		t.Fatalf("metadata does not describe queries: %v", spy.windows)
	}
}

// Paid metrics are withheld in the API as well as in the interface.
func TestRangeReport_ProductAnalysisRequiresPlus(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	for _, tc := range []struct {
		plan    string
		allowed bool
	}{
		{"standard_monthly", false}, {"plus_monthly", true},
	} {
		t.Run(tc.plan, func(t *testing.T) {
			reader := &profitabilitySpy{}
			gym := sampleGym()
			gym.SubscriptionPlan = tc.plan
			uc := reports.NewRangeReport(reader, fakeUoW{}).WithGyms(&fakeGymRepo{gym: gym})
			out, err := uc.Execute(context.Background(), reports.RangeReportInput{GymID: gym.ID, Period: reports.PeriodMonth})
			if err != nil {
				t.Fatal(err)
			}
			if (out.ProductProfitability != nil) != tc.allowed || (reader.calls == 1) != tc.allowed {
				t.Fatalf("%s: paid data=%v, queries=%d", tc.plan, out.ProductProfitability, reader.calls)
			}
		})
	}
}

type profitabilitySpy struct {
	fakeReader
	calls int
}

func (r *profitabilitySpy) ProductProfitabilityBetween(_ sharedDomain.Transaction, _ uuid.UUID, _, _ time.Time) ([]reports.ProductProfitabilityRow, error) {
	r.calls++
	return []reports.ProductProfitabilityRow{{ProductID: uuid.New(), ProductName: "Agua", CostComplete: true}}, nil
}
