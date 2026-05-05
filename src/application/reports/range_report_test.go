package reports_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/reports"
)

// TestRangeReport_PopulatesAllTotals exercises the UC-036 use case end-to-end
// against the fakeReader, verifying that the 6 new range-only queries reach
// the output. Fixture: 3 payments ($1500), 2 allowed checkins, 1 refund of
// $250, 1 new member.
func TestRangeReport_PopulatesAllTotals(t *testing.T) {
	memberID := uuid.New()
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
	}

	uc := reports.NewRangeReport(reader, fakeUoW{})
	out, err := uc.Execute(context.Background(), reports.RangeReportInput{
		GymID:  uuid.New(),
		Period: reports.PeriodMonth,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if out.Totals.Income != 1500 {
		t.Errorf("totals.income = %.2f, want 1500", out.Totals.Income)
	}
	if out.Totals.NewMembers != 1 {
		t.Errorf("totals.new_members = %d, want 1", out.Totals.NewMembers)
	}
	if out.Totals.Checkins != 2 {
		t.Errorf("totals.checkins = %d, want 2", out.Totals.Checkins)
	}
	if out.Totals.Refunds != 250 {
		t.Errorf("totals.refunds = %.2f, want 250", out.Totals.Refunds)
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

// TestRangeReport_PeriodWindow_Today returns a same-day from/to.
func TestRangeReport_PeriodWindow_Today(t *testing.T) {
	today := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	from, to := reports.PeriodWindow(reports.PeriodToday, today)
	if from.Format("2006-01-02") != "2026-04-27" || to.Format("2006-01-02") != "2026-04-27" {
		t.Errorf("today window = %s..%s", from, to)
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
	if out.Totals.Income != 0 || out.Totals.NewMembers != 0 || out.Totals.Checkins != 0 || out.Totals.Refunds != 0 {
		t.Errorf("expected zero totals, got %+v", out.Totals)
	}
	if out.IncomeByMethod == nil {
		t.Error("income_by_method should be non-nil for FE rendering")
	}
	if out.TopMembers == nil {
		t.Error("top_members should be non-nil")
	}
	if out.CheckinsByDay == nil {
		t.Error("checkins_by_day should be non-nil")
	}
}
