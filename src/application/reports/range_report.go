// UC-036 — Reportes por período. Composes the range page (4 KPIs +
// per-method breakdown + top members + 2 daily series) out of the Reader
// queries. The Reader is the same one the dashboard uses; main.go shares
// the singleton.
package reports

import (
	"context"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ReportPeriod mirrors the FE enum.
const (
	PeriodToday     = "today"
	PeriodWeek      = "week"
	PeriodMonth     = "month"
	PeriodLastMonth = "last_month"
	Period3Months   = "3_months"
	PeriodYear      = "year"
)

// topMembersLimit caps the leaderboard at 5 — same as the FE renders.
const topMembersLimit = 5

type RangeReportInput struct {
	GymID  uuid.UUID
	Period string
}

// RangeReportOutput matches the FE ReportsRangeData shape. Date strings
// (period range) come back YYYY-MM-DD; daily series rows mirror the
// dashboard chart shape.
type RangeReportOutput struct {
	Period         string             `json:"period"`
	From           string             `json:"from"`
	To             string             `json:"to"`
	Totals         RangeTotals        `json:"totals"`
	IncomeByDay    []DailyIncome      `json:"income_by_day"`
	CheckinsByDay  []DailyCount       `json:"checkins_by_day"`
	IncomeByMethod map[string]float64 `json:"income_by_method"`
	TopMembers     []TopMemberRow     `json:"top_members"`
}

type RangeTotals struct {
	Income     float64 `json:"income"`
	NewMembers int     `json:"new_members"`
	Checkins   int     `json:"checkins"`
	Refunds    float64 `json:"refunds"`
}

// DailyCount is `{date, count}[]` — used for the checkins-by-day chart.
type DailyCount struct {
	Date  time.Time `json:"date"`
	Count int       `json:"count"`
}

// TopMemberRow is one entry in the "top socios pagadores" leaderboard.
type TopMemberRow struct {
	MemberID      uuid.UUID `json:"member_id"`
	FullName      string    `json:"full_name"`
	TotalPaid     float64   `json:"total_paid"`
	PaymentsCount int       `json:"payments_count"`
}

// RangeReport is the UC-036 use case.
type RangeReport struct {
	Reader Reader
	UoW    sharedDomain.UnitOfWork
}

func NewRangeReport(reader Reader, uow sharedDomain.UnitOfWork) *RangeReport {
	return &RangeReport{Reader: reader, UoW: uow}
}

func (uc *RangeReport) Execute(ctx context.Context, in RangeReportInput) (*RangeReportOutput, error) {
	from, to := PeriodWindow(in.Period, time.Now().UTC())
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}

	out := &RangeReportOutput{
		Period:         in.Period,
		From:           from.Format("2006-01-02"),
		To:             to.Format("2006-01-02"),
		IncomeByDay:    []DailyIncome{},
		CheckinsByDay:  []DailyCount{},
		IncomeByMethod: map[string]float64{},
		TopMembers:     []TopMemberRow{},
	}

	// Income — sum payments in window (Reader excludes refunds).
	if income, err := uc.Reader.SumPaymentsBetween(tx, in.GymID, from, to); err == nil {
		out.Totals.Income = income
	}
	if series, err := uc.Reader.IncomeDailySeries(tx, in.GymID, from, to); err == nil && series != nil {
		out.IncomeByDay = series
	}

	// Range-only KPIs (UC-036). Errors degrade silently to zero — the page
	// is composed of independent cards, so a single broken query shouldn't
	// blank the whole report.
	if n, err := uc.Reader.CountNewMembersBetween(tx, in.GymID, from, to); err == nil {
		out.Totals.NewMembers = n
	}
	if n, err := uc.Reader.CountCheckinsBetween(tx, in.GymID, from, to); err == nil {
		out.Totals.Checkins = n
	}
	if v, err := uc.Reader.SumRefundsBetween(tx, in.GymID, from, to); err == nil {
		out.Totals.Refunds = v
	}
	if m, err := uc.Reader.IncomeByMethodBetween(tx, in.GymID, from, to); err == nil && m != nil {
		out.IncomeByMethod = m
	}
	if rows, err := uc.Reader.TopMembersBetween(tx, in.GymID, from, to, topMembersLimit); err == nil && rows != nil {
		out.TopMembers = rows
	}
	if rows, err := uc.Reader.CheckinsDailySeries(tx, in.GymID, from, to); err == nil && rows != nil {
		out.CheckinsByDay = rows
	}

	return out, nil
}

// PeriodWindow maps the FE-side period enum to a [from, to] range. Both
// ends inclusive; today is the upper bound for "today/week/month/3_months/
// year"; last_month covers the prior calendar month.
func PeriodWindow(period string, today time.Time) (time.Time, time.Time) {
	t := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case PeriodToday:
		return t, t
	case PeriodWeek:
		return t.AddDate(0, 0, -6), t
	case PeriodLastMonth:
		first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
		last := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
		return first, last
	case Period3Months:
		return t.AddDate(0, -3, 0), t
	case PeriodYear:
		return t.AddDate(-1, 0, 0), t
	default: // PeriodMonth
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), t
	}
}
