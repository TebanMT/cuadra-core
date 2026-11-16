//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	reports "github.com/cuadra/cuadra-core/src/application/reports"
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	cash "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	domain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// moveCashPaymentToDay books a sale on a past business day while keeping its
// real capture time, the way a late capture or a forgotten opening looks.
func moveCashPaymentToDay(t *testing.T, f *salesFixture, paymentID uuid.UUID, day time.Time) {
	t.Helper()
	if _, err := f.db.Exec("UPDATE payments SET payment_date=? WHERE id=?", day.Format("2006-01-02"), paymentID); err != nil {
		t.Fatalf("move payment to %s: %v", day.Format("2006-01-02"), err)
	}
}

func reviewCashDay(f *salesFixture, uc *reports.CashClose, role string, day time.Time, note string) (*reports.CashSessionView, error) {
	return uc.ReviewDay(context.Background(), reports.CashReviewDayInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: role, Date: day, Note: note,
	})
}

func cashDayReport(t *testing.T, f *salesFixture, uc *reports.CashClose, day time.Time) *reports.CashCloseReportOutput {
	t.Helper()
	out, err := uc.Report(context.Background(), reports.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil {
		t.Fatalf("report %s: %v", day.Format("2006-01-02"), err)
	}
	return out
}

func periodCashSummary(t *testing.T, f *salesFixture, day time.Time) reports.CashCountSummary {
	t.Helper()
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := reportsInfra.NewSQLiteReader().SumCashCountedBetween(tx, f.gymID, day, day)
	if err != nil {
		t.Fatalf("period summary: %v", err)
	}
	return summary
}

func TestCashReview_ForgottenDayClosesWithoutCountAndLapsesOnLateCash(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	today := cashSessionBusinessDay(t)
	yesterday := today.AddDate(0, 0, -1)
	product := f.seedProduct(t, "Agua", 20, 10)
	moveCashPaymentToDay(t, f, registerCashProductSale(t, f, product), yesterday)

	report := cashDayReport(t, f, uc, yesterday)
	if report.Session != nil || !report.RequiresNewSession || !report.PendingReview {
		t.Fatalf("forgotten day = session %+v requiresNew %v pending %v", report.Session, report.RequiresNewSession, report.PendingReview)
	}
	if cashDayReport(t, f, uc, today).PendingReview {
		t.Fatal("today is closed with a count, never reviewed")
	}

	if _, err := reviewCashDay(f, uc, "operator", yesterday, "se olvidó"); !errors.Is(err, cash.ErrReviewOwner) {
		t.Fatalf("operator review err = %v, want ErrReviewOwner", err)
	}
	if _, err := reviewCashDay(f, uc, "owner", today, "hoy"); !errors.Is(err, cash.ErrReviewPastDay) {
		t.Fatalf("today review err = %v, want ErrReviewPastDay", err)
	}
	if _, err := reviewCashDay(f, uc, "owner", yesterday, "   "); !errors.Is(err, cash.ErrReviewNote) {
		t.Fatalf("blank note err = %v, want ErrReviewNote", err)
	}
	if cashDayReport(t, f, uc, yesterday).Session != nil {
		t.Fatal("a rejected review must not leave a session behind")
	}

	view, err := reviewCashDay(f, uc, "owner", yesterday, "Se olvidó abrir la caja")
	if err != nil {
		t.Fatalf("review forgotten day: %v", err)
	}
	if !view.ReviewedWithoutCount || view.Status != cash.StatusClosedUnverified || view.OpeningCashKnown ||
		view.ActivityCash != 20 || view.CountedCash != nil || view.Difference != nil {
		t.Fatalf("reviewed session = %+v", view)
	}
	report = cashDayReport(t, f, uc, yesterday)
	if report.PendingReview || report.RequiresNewSession || report.Session == nil || !report.Session.ReviewedWithoutCount ||
		report.Session.ReviewNote == nil || *report.Session.ReviewNote != "Se olvidó abrir la caja" {
		t.Fatalf("reviewed day report = %+v", report.Session)
	}
	if _, err := reviewCashDay(f, uc, "owner", yesterday, "otra vez"); !errors.Is(err, cash.ErrNothingToReview) {
		t.Fatalf("second review err = %v, want ErrNothingToReview", err)
	}

	// Cash captured later for that day reopens the question.
	time.Sleep(2 * time.Millisecond)
	moveCashPaymentToDay(t, f, registerCashProductSale(t, f, product), yesterday)
	report = cashDayReport(t, f, uc, yesterday)
	if !report.PendingReview || report.Session.ReviewedWithoutCount || report.Session.Status != cash.StatusStale {
		t.Fatalf("late cash after review = pending %v %+v", report.PendingReview, report.Session)
	}
	view, err = reviewCashDay(f, uc, "owner", yesterday, "Venta capturada tarde")
	if err != nil {
		t.Fatalf("review after late cash: %v", err)
	}
	if view.ID != report.Session.ID || view.Sequence != 1 || view.ActivityCash != 40 || !view.ReviewedWithoutCount {
		t.Fatalf("re-reviewed session = %+v", view)
	}
}

func TestCashReview_SessionLeftOpenClosesWithItsOpening(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	yesterday := cashSessionBusinessDay(t).AddDate(0, 0, -1)
	var opened *cash.CashCloseEvent
	if err := f.uow.Command(context.Background(), func(tx domain.Transaction) error {
		var err error
		opened, err = cash.Open(f.gymID, f.gymID, f.ownerID, yesterday, 1, 100, time.Now().Add(-25*time.Hour))
		if err != nil {
			return err
		}
		_, err = f.cashCloseEvents.Create(tx, opened)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	product := f.seedProduct(t, "Agua", 20, 10)
	moveCashPaymentToDay(t, f, registerCashProductSale(t, f, product), yesterday)
	if summary := periodCashSummary(t, f, yesterday); summary.OpenSessions != 1 {
		t.Fatalf("summary before review = %+v", summary)
	}
	if report := cashDayReport(t, f, uc, yesterday); !report.PendingReview || report.Session.Status != cash.StatusOpen {
		t.Fatalf("open past session = pending %v %+v", report.PendingReview, report.Session)
	}

	view, err := reviewCashDay(f, uc, "owner", yesterday, "Nadie hizo el corte")
	if err != nil {
		t.Fatalf("review open session: %v", err)
	}
	if view.ID != opened.ID || view.Status != cash.StatusClosedUnverified || !view.OpeningCashKnown ||
		view.ExpectedCash != 120 || !view.ReviewedWithoutCount {
		t.Fatalf("reviewed open session = %+v", view)
	}
	summary := periodCashSummary(t, f, yesterday)
	if summary.ReviewedSessions != 1 || summary.ClosedUnverifiedSessions != 0 || summary.OpenSessions != 0 ||
		summary.UnknownOpeningSessions != 0 || summary.UncoveredActivityDays != 0 || summary.MissingActiveDays != 0 ||
		summary.TotalCloses != 1 || summary.CountedCloses != 0 {
		t.Fatalf("summary after review = %+v", summary)
	}
}

func TestCashReview_CashAfterFinishedCountGetsItsOwnReviewedSession(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	yesterday := cashSessionBusinessDay(t).AddDate(0, 0, -1)
	var finished *cash.CashCloseEvent
	if err := f.uow.Command(context.Background(), func(tx domain.Transaction) error {
		var err error
		finished, err = cash.Open(f.gymID, f.gymID, f.ownerID, yesterday, 1, 80, time.Now().Add(-25*time.Hour))
		if err != nil {
			return err
		}
		at := time.Now().Add(-24 * time.Hour)
		if err = finished.Close(0, f.ownerID, at); err != nil {
			return err
		}
		if err = finished.Reconcile(80, nil, f.ownerID, at); err != nil {
			return err
		}
		if err = finished.Finish(at); err != nil {
			return err
		}
		_, err = f.cashCloseEvents.Create(tx, finished)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	product := f.seedProduct(t, "Agua", 20, 10)
	moveCashPaymentToDay(t, f, registerCashProductSale(t, f, product), yesterday)

	// "Terminar sin retirar" bounds the count at reconciled_at: the late sale
	// belongs to no session yet, and the finished count is still valid.
	summary := periodCashSummary(t, f, yesterday)
	if summary.StaleSessions != 0 || summary.ReconciledSessions != 1 || summary.UncoveredActivityDays != 1 {
		t.Fatalf("summary before review = %+v", summary)
	}
	report := cashDayReport(t, f, uc, yesterday)
	if !report.PendingReview || !report.RequiresNewSession || report.Session.FinishedAt == nil {
		t.Fatalf("cash after finish = pending %v requiresNew %v %+v", report.PendingReview, report.RequiresNewSession, report.Session)
	}

	view, err := reviewCashDay(f, uc, "owner", yesterday, "Cobro después del corte")
	if err != nil {
		t.Fatalf("review cash after finish: %v", err)
	}
	if view.ID == finished.ID || view.Sequence != 2 || view.OpeningCash != 80 || !view.OpeningCashKnown ||
		view.ActivityCash != 20 || view.ExpectedCash != 100 || !view.ReviewedWithoutCount {
		t.Fatalf("follow-up session = %+v", view)
	}
	report = cashDayReport(t, f, uc, yesterday)
	if report.PendingReview || report.RequiresNewSession || len(report.Sessions) != 2 {
		t.Fatalf("day after review = pending %v requiresNew %v sessions %d", report.PendingReview, report.RequiresNewSession, len(report.Sessions))
	}
	summary = periodCashSummary(t, f, yesterday)
	if summary.StaleSessions != 0 || summary.ReconciledSessions != 1 || summary.ReviewedSessions != 1 ||
		summary.ClosedUnverifiedSessions != 0 || summary.UncoveredActivityDays != 0 ||
		summary.TotalCloses != 2 || summary.CountedCloses != 1 {
		t.Fatalf("summary after review = %+v", summary)
	}
}
