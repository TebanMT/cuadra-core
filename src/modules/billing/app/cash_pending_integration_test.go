//go:build sidecar

package app_test

import (
	"context"
	"testing"
	"time"

	reports "github.com/cuadra/cuadra-core/src/application/reports"
	cash "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	domain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func pendingCashDays(t *testing.T, f *salesFixture, uc *reports.CashClose, includeToday bool) []reports.CashPendingDay {
	t.Helper()
	out, err := uc.PendingDays(context.Background(), reports.CashPendingDaysInput{GymID: f.gymID, IncludeToday: includeToday})
	if err != nil {
		t.Fatalf("pending days: %v", err)
	}
	return out
}

func seedOpenCashSession(t *testing.T, f *salesFixture, day time.Time, opening float64, openedAt time.Time) *cash.CashCloseEvent {
	t.Helper()
	var session *cash.CashCloseEvent
	if err := f.uow.Command(context.Background(), func(tx domain.Transaction) error {
		var err error
		session, err = cash.Open(f.gymID, f.gymID, f.ownerID, day, 1, opening, openedAt)
		if err != nil {
			return err
		}
		_, err = f.cashCloseEvents.Create(tx, session)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return session
}

func TestCashPending_GymThatNeverOpenedTheDrawerIsNotNagged(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	yesterday := cashSessionBusinessDay(t).AddDate(0, 0, -1)
	product := f.seedProduct(t, "Agua", 20, 10)
	moveCashPaymentToDay(t, f, registerCashProductSale(t, f, product), yesterday)
	if got := pendingCashDays(t, f, uc, true); len(got) != 0 {
		t.Fatalf("untracked gym pending = %+v, want none", got)
	}
}

func TestCashPending_DrawerLeftOpenCanBeCountedTheNextMorning(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	today := cashSessionBusinessDay(t)
	yesterday := today.AddDate(0, 0, -1)
	session := seedOpenCashSession(t, f, yesterday, 100, time.Now().Add(-25*time.Hour))
	product := f.seedProduct(t, "Agua", 20, 10)
	moveCashPaymentToDay(t, f, registerCashProductSale(t, f, product), yesterday)

	got := pendingCashDays(t, f, uc, false)
	if len(got) != 1 {
		t.Fatalf("pending = %+v, want yesterday only", got)
	}
	day := got[0]
	if !day.Date.Equal(yesterday) || day.Reason != reports.CashPendingOpen || !day.CountNow || !day.IsMainDrawer ||
		day.ExpectedCash == nil || *day.ExpectedCash != 120 || day.Session == nil || day.Session.ID != session.ID {
		t.Fatalf("pending day = %+v", day)
	}
	if report := cashDayReport(t, f, uc, yesterday); !report.PendingReview {
		t.Fatal("pending days must agree with the day report")
	}

	// Counting the untouched drawer in the morning is that day's corte.
	expected, counted := 120.0, 120.0
	if _, err := uc.Close(context.Background(), reports.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: yesterday, SessionID: session.ID, Finish: true,
		ExpectedCash: &expected, CountedCash: &counted,
	}); err != nil {
		t.Fatalf("count yesterday this morning: %v", err)
	}
	if got := pendingCashDays(t, f, uc, true); len(got) != 0 {
		t.Fatalf("pending after counting = %+v", got)
	}
}

func TestCashPending_TodayActivityMeansTheOldCountIsGone(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	today := cashSessionBusinessDay(t)
	yesterday := today.AddDate(0, 0, -1)
	twoDaysAgo := today.AddDate(0, 0, -2)
	seedOpenCashSession(t, f, twoDaysAgo, 100, time.Now().Add(-49*time.Hour))
	product := f.seedProduct(t, "Agua", 20, 10)
	// Two days ago left open; yesterday nobody opened; today a sale too.
	moveCashPaymentToDay(t, f, registerCashProductSale(t, f, product), yesterday)
	registerCashProductSale(t, f, product)

	got := pendingCashDays(t, f, uc, false)
	if len(got) != 2 {
		t.Fatalf("pending = %+v, want yesterday and two days ago", got)
	}
	if !got[0].Date.Equal(yesterday) || got[0].Reason != reports.CashPendingNoOpening || got[0].UncoveredCash != 20 || got[0].CountNow {
		t.Fatalf("yesterday = %+v", got[0])
	}
	if !got[1].Date.Equal(twoDaysAgo) || got[1].Reason != reports.CashPendingOpen || got[1].CountNow {
		t.Fatalf("two days ago = %+v, the drawer moved since then", got[1])
	}

	withToday := pendingCashDays(t, f, uc, true)
	if len(withToday) != 3 || !withToday[0].Date.Equal(today) || !withToday[0].IsToday ||
		withToday[0].Reason != reports.CashPendingNoOpening || withToday[0].CountNow || withToday[1].IsToday {
		t.Fatalf("with today = %+v", withToday)
	}
}

func TestCashPending_OwnerReviewClearsTheReminder(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	yesterday := cashSessionBusinessDay(t).AddDate(0, 0, -1)
	seedOpenCashSession(t, f, yesterday, 100, time.Now().Add(-25*time.Hour))
	if got := pendingCashDays(t, f, uc, false); len(got) != 1 || got[0].Reason != reports.CashPendingOpen {
		t.Fatalf("open session without sales = %+v, an opened drawer still needs its corte", got)
	}
	if _, err := reviewCashDay(f, uc, "owner", yesterday, "Nadie cobró en efectivo"); err != nil {
		t.Fatal(err)
	}
	if got := pendingCashDays(t, f, uc, false); len(got) != 0 {
		t.Fatalf("pending after owner review = %+v", got)
	}
}
