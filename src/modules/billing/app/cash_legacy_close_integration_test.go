//go:build sidecar

package app_test

import (
	"context"
	"testing"
	"time"

	reports "github.com/cuadra/cuadra-core/src/application/reports"
)

// An old client closes without opening the drawer and without saying how much
// cash it started with. Tinta must not invent a $0 opening: the count stays
// visible, but it claims no difference and the owner is told what is missing.
func TestCashLegacyClose_WithoutAnOpeningInventsNoDifference(t *testing.T) {
	f := setupSales(t)
	day := cashSessionBusinessDay(t)
	// The gym already tracks its drawer: yesterday it was opened with a count.
	seedOpenCashSession(t, f, day.AddDate(0, 0, -1), 100, time.Now().Add(-25*time.Hour))
	registerCashProductSale(t, f, f.seedProduct(t, "Agua", 20, 5))
	events := &recordingCashSubscriber{}
	uc := countedPeriodUC(f).WithSubscriber(events)

	// $500 of float nobody recorded plus the $20 sale.
	counted := 520.0
	out, err := uc.Close(context.Background(), reports.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, CountedCash: &counted,
	})
	if err != nil {
		t.Fatalf("legacy close without a reason for the gap: %v", err)
	}
	if out.Session == nil || out.Session.OpeningCashKnown || out.Discrepancy != nil || out.Session.Difference != nil {
		t.Fatalf("legacy close = %+v session=%+v", out, out.Session)
	}
	if len(events.events) != 1 || events.events[0].Discrepancy != nil {
		t.Fatalf("an unknown opening must not alert a difference: %+v", events.events)
	}

	summary := periodCashSummary(t, f, day)
	if len(summary.DifferenceSessions) != 0 || summary.LatestDifference != nil || summary.LatestExpected != nil {
		t.Fatalf("period invented a difference: list=%+v latest=%v expected=%v",
			summary.DifferenceSessions, summary.LatestDifference, summary.LatestExpected)
	}
	if summary.UnknownOpeningSessions != 1 || summary.LatestCounted == nil || *summary.LatestCounted != 520 {
		t.Fatalf("period must keep the count and flag the missing opening: %+v", summary)
	}

	// A gym that only ever used the old client never started tracking: its
	// counts read as history, like migrated cuts, never as a certified count.
	legacyOnly := setupSales(t)
	registerCashProductSale(t, legacyOnly, legacyOnly.seedProduct(t, "Agua", 20, 5))
	if _, err := countedPeriodUC(legacyOnly).Close(context.Background(), reports.CashCloseInput{
		GymID: legacyOnly.gymID, ActorUserID: legacyOnly.ownerID, Date: day, CountedCash: &counted,
	}); err != nil {
		t.Fatal(err)
	}
	if got := periodCashSummary(t, legacyOnly, day); got.HistoricalSessions != 1 || got.TotalSessions != 0 ||
		len(got.DifferenceSessions) != 0 || got.LatestDifference != nil {
		t.Fatalf("legacy-only gym summary = %+v", got)
	}

	// When the old client does send the opening, the count is trusted as before.
	f2 := setupSales(t)
	registerCashProductSale(t, f2, f2.seedProduct(t, "Agua", 20, 5))
	opening, short, reason := 500.0, 515.0, "faltó cambio"
	known, err := countedPeriodUC(f2).Close(context.Background(), reports.CashCloseInput{
		GymID: f2.gymID, ActorUserID: f2.ownerID, Date: day, OpeningCash: &opening,
		CountedCash: &short, DiscrepancyReason: &reason,
	})
	if err != nil || !known.Session.OpeningCashKnown || known.Discrepancy == nil || *known.Discrepancy != -5 {
		t.Fatalf("legacy close with opening = %+v err=%v", known, err)
	}
	if got := periodCashSummary(t, f2, day).DifferenceSessions; len(got) != 1 || got[0].Difference != -5 {
		t.Fatalf("known opening difference = %+v", got)
	}
}
