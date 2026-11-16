//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	reports "github.com/cuadra/cuadra-core/src/application/reports"
	cash "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
)

// countYesterday leaves yesterday's drawer counted and finished with the cash
// it opened with, as "Terminar sin retirar" does.
func countYesterday(t *testing.T, f *salesFixture, uc *reports.CashClose, cashLeft float64) time.Time {
	t.Helper()
	yesterday := cashSessionBusinessDay(t).AddDate(0, 0, -1)
	session := seedOpenCashSession(t, f, yesterday, cashLeft, time.Now().Add(-25*time.Hour))
	expected, counted := cashLeft, cashLeft
	if _, err := uc.Close(context.Background(), reports.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: yesterday, SessionID: session.ID, Finish: true,
		ExpectedCash: &expected, CountedCash: &counted,
	}); err != nil {
		t.Fatalf("count yesterday: %v", err)
	}
	return yesterday
}

func openToday(f *salesFixture, uc *reports.CashClose, opening float64, note string) (*reports.CashSessionView, error) {
	return uc.Open(context.Background(), reports.CashOpenInput{
		GymID: f.gymID, ActorUserID: f.ownerID, OpeningCash: opening, OpeningNote: note, Sequence: 1,
	})
}

func TestCashOpening_OvernightShortageNeedsANoteAndReachesTheOwner(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	today := cashSessionBusinessDay(t)
	yesterday := countYesterday(t, f, uc, 500)

	report := cashDayReport(t, f, uc, today)
	ref := report.OpeningReference
	if ref == nil || !ref.LastCountDate.Equal(yesterday) || ref.CashLeft != 500 || ref.LaterCash != 0 || ref.Expected != 500 ||
		report.SuggestedOpeningCash == nil || *report.SuggestedOpeningCash != 500 {
		t.Fatalf("reference=%+v suggested=%v", ref, report.SuggestedOpeningCash)
	}

	// Ayer quedaron $500, hoy empieza con $450: allowed, but explained.
	if _, err := openToday(f, uc, 450, "  "); !errors.Is(err, cash.ErrOpeningNote) {
		t.Fatalf("opening short without a note = %v", err)
	}
	session, err := openToday(f, uc, 450, "Faltaban  50 al llegar")
	if err != nil {
		t.Fatalf("opening short with a note: %v", err)
	}
	if session.OpeningExpectedCash == nil || *session.OpeningExpectedCash != 500 ||
		session.OpeningDifference == nil || *session.OpeningDifference != -50 ||
		session.OpeningNote == nil || *session.OpeningNote != "Faltaban 50 al llegar" || session.ExpectedCash != 450 {
		t.Fatalf("opened session = %+v", session)
	}
	if retry, err := openToday(f, uc, 450, ""); err != nil || retry.ID != session.ID {
		t.Fatalf("retrying the same opening = %+v err=%v", retry, err)
	}
	if stored := cashDayReport(t, f, uc, today).Session; stored == nil || stored.OpeningDifference == nil || *stored.OpeningDifference != -50 {
		t.Fatalf("stored session = %+v", stored)
	}

	summary := periodCashSummary(t, f, today)
	if len(summary.OpeningDifferences) != 1 {
		t.Fatalf("period opening differences = %+v", summary.OpeningDifferences)
	}
	got := summary.OpeningDifferences[0]
	if got.SessionID != session.ID || got.Date != today.Format("2006-01-02") || got.Difference != -50 ||
		got.Note == nil || *got.Note != "Faltaban 50 al llegar" {
		t.Fatalf("period opening difference = %+v", got)
	}
}

func TestCashOpening_CashAfterTheCountIsPartOfWhatShouldBeThere(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	today := cashSessionBusinessDay(t)
	yesterday := countYesterday(t, f, uc, 120)
	time.Sleep(5 * time.Millisecond)
	product := f.seedProduct(t, "Agua", 20, 10)
	// A sale after the final count stayed in the drawer overnight.
	moveCashPaymentToDay(t, f, registerCashProductSale(t, f, product), yesterday)

	ref := cashDayReport(t, f, uc, today).OpeningReference
	if ref == nil || ref.CashLeft != 120 || ref.LaterCash != 20 || ref.Expected != 140 {
		t.Fatalf("reference = %+v", ref)
	}
	session, err := openToday(f, uc, 140, "")
	if err != nil {
		t.Fatalf("matching opening: %v", err)
	}
	if session.OpeningExpectedCash == nil || *session.OpeningExpectedCash != 140 || session.OpeningDifference != nil || session.OpeningNote != nil {
		t.Fatalf("matching opening session = %+v", session)
	}
	if got := periodCashSummary(t, f, today).OpeningDifferences; len(got) != 0 {
		t.Fatalf("a matching opening is not a difference: %+v", got)
	}
}

func TestCashOpening_WithoutAFinishedCountThereIsNothingToCompare(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	today := cashSessionBusinessDay(t)
	// Yesterday's drawer was left open: the morning banner handles it.
	seedOpenCashSession(t, f, today.AddDate(0, 0, -1), 500, time.Now().Add(-25*time.Hour))
	if ref := cashDayReport(t, f, uc, today).OpeningReference; ref != nil {
		t.Fatalf("reference from an unfinished session = %+v", ref)
	}
	session, err := openToday(f, uc, 300, "")
	if err != nil {
		t.Fatalf("opening without reference: %v", err)
	}
	if session.OpeningExpectedCash != nil || session.OpeningDifference != nil {
		t.Fatalf("unchecked opening = %+v", session)
	}
}

func TestCashOpening_ARecountThatEndsMatchingKeepsTheFirstCount(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	today := cashSessionBusinessDay(t)
	countYesterday(t, f, uc, 500)

	// Staff first counted $450, saw the reference and recounted $500.
	session, err := openToday(f, uc, 500, "Primer conteo: $450.00.")
	if err != nil {
		t.Fatalf("matching opening with a note: %v", err)
	}
	if session.OpeningDifference != nil || session.OpeningNote == nil || *session.OpeningNote != "Primer conteo: $450.00." {
		t.Fatalf("opened session = %+v", session)
	}
	if stored := cashDayReport(t, f, uc, today).Session; stored == nil || stored.OpeningNote == nil {
		t.Fatalf("stored session lost the note: %+v", stored)
	}
	if got := periodCashSummary(t, f, today).OpeningDifferences; len(got) != 0 {
		t.Fatalf("a matching opening is not a difference: %+v", got)
	}
}
