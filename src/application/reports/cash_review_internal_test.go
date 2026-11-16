package reports

import (
	"testing"
	"time"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
)

func TestCashReconciliationCoverage_ReviewedSessionsStopAttentionWithoutCertifying(t *testing.T) {
	counted := CashCountSummary{ActiveDays: 2, ReconciledSessions: 1, TotalSessions: 1, CountedCloses: 1, TotalCloses: 1}
	if status, complete, attention := cashReconciliationCoverage(counted, 1, 1); status != "complete" || !complete || attention {
		t.Fatalf("fully counted = %s %v %v", status, complete, attention)
	}

	reviewed := counted
	reviewed.ReviewedSessions = 1
	reviewed.TotalSessions, reviewed.TotalCloses = 2, 2
	if status, complete, attention := cashReconciliationCoverage(reviewed, 2, 1); status != "incomplete" || complete || attention {
		t.Fatalf("with a reviewed day = %s %v %v, want incomplete without attention", status, complete, attention)
	}

	pending := reviewed
	pending.ClosedUnverifiedSessions = 1
	pending.TotalSessions, pending.TotalCloses = 3, 3
	if status, complete, attention := cashReconciliationCoverage(pending, 3, 1); status != "incomplete" || complete || !attention {
		t.Fatalf("with an unreviewed close = %s %v %v, want attention", status, complete, attention)
	}

	uncovered := reviewed
	uncovered.UncoveredActivityDays = 1
	if _, _, attention := cashReconciliationCoverage(uncovered, 2, 1); !attention {
		t.Fatal("cash after a review still needs attention")
	}
}

func TestCashDayNeedsReview_OffersOnlyWhatReviewDayResolves(t *testing.T) {
	finished := time.Date(2026, 11, 11, 23, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		out  CashCloseReportOutput
		want bool
	}{
		{"no session, no cash", CashCloseReportOutput{}, false},
		{"no session, cash", CashCloseReportOutput{RequiresNewSession: true}, true},
		{"open", CashCloseReportOutput{Session: &CashSessionView{Status: cashCloseDomain.StatusOpen}}, true},
		{"stale", CashCloseReportOutput{Session: &CashSessionView{Status: cashCloseDomain.StatusStale}}, true},
		{"closed without count", CashCloseReportOutput{Session: &CashSessionView{Status: cashCloseDomain.StatusClosedUnverified}}, true},
		{"already reviewed", CashCloseReportOutput{Session: &CashSessionView{Status: cashCloseDomain.StatusClosedUnverified, ReviewedWithoutCount: true}}, false},
		{"counted", CashCloseReportOutput{Session: &CashSessionView{Status: cashCloseDomain.StatusReconciled}}, false},
		{"finished", CashCloseReportOutput{Session: &CashSessionView{Status: cashCloseDomain.StatusWithdrawn, FinishedAt: &finished}}, false},
		{"cash after finish", CashCloseReportOutput{RequiresNewSession: true, Session: &CashSessionView{Status: cashCloseDomain.StatusWithdrawn, FinishedAt: &finished}}, true},
	}
	for _, tc := range cases {
		if got := cashDayNeedsReview(&tc.out); got != tc.want {
			t.Errorf("%s: cashDayNeedsReview = %v, want %v", tc.name, got, tc.want)
		}
	}
}
