//go:build sidecar

package app_test

import (
	"context"
	"testing"
	"time"

	reports "github.com/cuadra/cuadra-core/src/application/reports"
	cash "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	repo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	domain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func countedPeriodUC(f *salesFixture) *reports.CashClose {
	return reports.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).WithGyms(f.gymRepo).WithUsers(userRepo.NewUserSQLiteRepository())
}

func TestCashPeriod_CloseWithoutWithdrawalFreezesCountAndNextPeriodCarriesActualCash(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	ctx := context.Background()
	day := cashSessionBusinessDay(t)
	product := f.seedProduct(t, "Agua", 20, 20)
	opening, err := uc.Open(ctx, reports.CashOpenInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, OpeningCash: 100, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := uc.Open(ctx, reports.CashOpenInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, OpeningCash: 100, Sequence: 1})
	if err != nil || retry.ID != opening.ID {
		t.Fatalf("opening retry=%+v err=%v", retry, err)
	}
	registerCashProductSale(t, f, product)
	counted := 115.0
	expected := 120.0
	reason := "Pendiente de aclarar"
	input := reports.CashCloseInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, SessionID: opening.ID, Finish: true, ExpectedCash: &expected, CountedCash: &counted, DiscrepancyReason: &reason}
	first, err := uc.Close(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Session.FinishedAt == nil || first.Session.WithdrawnAt != nil || first.Session.CashLeft == nil || *first.Session.CashLeft != 115 || first.Discrepancy == nil || *first.Discrepancy != -5 {
		t.Fatalf("finished without transfer=%+v", first.Session)
	}
	retryClose, err := uc.Close(ctx, input)
	if err != nil || retryClose.CashCloseID != first.CashCloseID {
		t.Fatalf("close retry=%+v %v", retryClose, err)
	}
	var transfers, expenses int
	if err = f.db.Get(&transfers, "SELECT COUNT(*) FROM cash_transfers WHERE gym_id=?", f.gymID); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Get(&expenses, "SELECT COUNT(*) FROM expenses WHERE gym_id=?", f.gymID); err != nil {
		t.Fatal(err)
	}
	if transfers != 0 || expenses != 0 {
		t.Fatalf("difference or retained cash created transfer/expense: %d/%d", transfers, expenses)
	}
	time.Sleep(3 * time.Millisecond)
	registerCashProductSale(t, f, product)
	report, err := uc.Report(ctx, reports.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil {
		t.Fatal(err)
	}
	if report.Session.ExpectedCash != 120 || *report.Session.CountedCash != 115 || !report.RequiresNewSession || report.UncoveredCashActivity != 20 || report.Session.IsStale {
		t.Fatalf("late activity rewrote count: %+v", report)
	}
	if len(report.Entries) != 2 || report.Session.ClosedByName == nil {
		t.Fatalf("missing ledger/operator: %+v", report)
	}
	tx, _ := f.uow.Query(ctx)
	coverage, err := f.cashCloseReader.SessionCoverage(tx, repo.CashSessionCoverageQuery{GymID: f.gymID, From: day, To: day})
	if err != nil || coverage.Complete {
		t.Fatalf("late activity must be uncovered: %+v %v", coverage, err)
	}
	if _, err = uc.Withdraw(ctx, reports.CashWithdrawInput{GymID: f.gymID, ActorUserID: f.ownerID, SessionID: first.CashCloseID, CashLeft: 0}); err == nil {
		t.Fatal("old count cannot authorize withdrawal after a new sale")
	}
	second, err := uc.Open(ctx, reports.CashOpenInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, OpeningCash: 115, Sequence: 2})
	if err != nil {
		t.Fatal(err)
	}
	counted = 135
	expected = 135
	out, err := uc.Close(ctx, reports.CashCloseInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, SessionID: second.ID, Finish: true, CountedCash: &counted, ExpectedCash: &expected})
	if err != nil {
		t.Fatal(err)
	}
	if out.Session.OpeningCash != 115 || out.Session.ActivityCash != 20 || out.CalculatedCash != 135 {
		t.Fatalf("double counted old activity: %+v", out.Session)
	}
	// Replaying the old request after another period closed still resolves its original ID.
	counted = 115
	expected = 120
	if replay, err := uc.Close(ctx, input); err != nil || replay.CashCloseID != first.CashCloseID {
		t.Fatalf("late retry=%+v %v", replay, err)
	}
	tx, _ = f.uow.Query(ctx)
	coverage, err = f.cashCloseReader.SessionCoverage(tx, repo.CashSessionCoverageQuery{GymID: f.gymID, From: day, To: day})
	if err != nil || !coverage.Complete || coverage.ActiveSessions != 2 {
		t.Fatalf("coverage=%+v err=%v", coverage, err)
	}
}

func TestCashPeriod_RejectsChangedExpectedCashAndCrossGymSession(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	ctx := context.Background()
	day := cashSessionBusinessDay(t)
	session, err := uc.Open(ctx, reports.CashOpenInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, OpeningCash: 100, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	product := f.seedProduct(t, "Barra", 20, 5)
	registerCashProductSale(t, f, product)
	old := 100.0
	input := reports.CashCloseInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, SessionID: session.ID, Finish: true, ExpectedCash: &old, CountedCash: &old}
	if _, err := uc.Close(ctx, input); err == nil {
		t.Fatal("stale expected amount accepted")
	}
	tx, _ := f.uow.Query(ctx)
	stored, _ := f.cashCloseEvents.GetByID(tx, f.gymID, session.ID)
	if stored.Status != cash.StatusOpen || stored.CountedCash != nil {
		t.Fatalf("failed close wrote data: %+v", stored)
	}
	input.SessionID = uuid.New()
	if _, err := uc.Close(ctx, input); err == nil {
		t.Fatal("unknown session accepted")
	}
	if _, err := uc.Open(ctx, reports.CashOpenInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, OpeningCash: 0, Sequence: 2}); err == nil {
		t.Fatal("overlapping period accepted")
	}
}

func TestCashPeriod_OpeningSuggestionUsesPreviousDrawerAndNeedsConfirmation(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	ctx := context.Background()
	today := cashSessionBusinessDay(t)
	yesterday := today.AddDate(0, 0, -1)
	var previous *cash.CashCloseEvent
	err := f.uow.Command(ctx, func(tx domain.Transaction) error {
		var err error
		previous, err = cash.Open(f.gymID, f.gymID, f.ownerID, yesterday, 1, 80, time.Now().Add(-25*time.Hour))
		if err != nil {
			return err
		}
		if err = previous.Close(0, f.ownerID, time.Now().Add(-24*time.Hour)); err != nil {
			return err
		}
		if err = previous.Reconcile(80, nil, f.ownerID, time.Now().Add(-24*time.Hour)); err != nil {
			return err
		}
		if err = previous.Finish(time.Now().Add(-24 * time.Hour)); err != nil {
			return err
		}
		_, err = f.cashCloseEvents.Create(tx, previous)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := uc.Report(ctx, reports.CashCloseReportInput{GymID: f.gymID, Date: today})
	if err != nil {
		t.Fatal(err)
	}
	if report.Session != nil || report.SuggestedOpeningCash == nil || *report.SuggestedOpeningCash != 80 {
		t.Fatalf("suggestion creates no opening: %+v", report)
	}
	other := uuid.New()
	seedCustomCashDrawer(t, f, other, "Otra")
	report, err = uc.Report(ctx, reports.CashCloseReportInput{GymID: f.gymID, Date: today, DrawerID: &other})
	if err != nil {
		t.Fatal(err)
	}
	if report.SuggestedOpeningCash != nil {
		t.Fatal("suggestion leaked from another drawer")
	}
	opened, err := uc.Open(ctx, reports.CashOpenInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: today, OpeningCash: 70, Sequence: 1})
	if err != nil || opened.OpeningCash != 70 {
		t.Fatalf("explicit next-day correction rejected: %+v %v", opened, err)
	}
}

func TestCashPeriod_WithdrawalAfterCountIsExplicitAndIdempotent(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	ctx := context.Background()
	day := cashSessionBusinessDay(t)
	opened, err := uc.Open(ctx, reports.CashOpenInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, OpeningCash: 100, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	count := 100.0
	closed, err := uc.Close(ctx, reports.CashCloseInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, SessionID: opened.ID, Finish: true, CountedCash: &count})
	if err != nil {
		t.Fatal(err)
	}
	input := reports.CashWithdrawInput{GymID: f.gymID, ActorUserID: f.ownerID, SessionID: closed.CashCloseID, CashLeft: 30}
	for i := 0; i < 2; i++ {
		out, err := uc.Withdraw(ctx, input)
		if err != nil || out.WithdrawnCash == nil || *out.WithdrawnCash != 70 {
			t.Fatalf("withdraw retry=%+v err=%v", out, err)
		}
	}
	var transfers int
	f.db.Get(&transfers, "SELECT COUNT(*) FROM cash_transfers WHERE gym_id=?", f.gymID)
	if transfers != 1 {
		t.Fatalf("transfers=%d", transfers)
	}
}

func TestCashPeriod_HistoricalCorrectionRetainsBoundaryAndCount(t *testing.T) {
	f := setupSales(t)
	uc := countedPeriodUC(f)
	ctx := context.Background()
	day := cashSessionBusinessDay(t)
	p := f.seedProduct(t, "Agua", 20, 5)
	opened, err := uc.Open(ctx, reports.CashOpenInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, OpeningCash: 0, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	payment := registerCashProductSale(t, f, p)
	count := 20.0
	first, err := uc.Close(ctx, reports.CashCloseInput{GymID: f.gymID, ActorUserID: f.ownerID, Date: day, SessionID: opened.ID, Finish: true, CountedCash: &count})
	if err != nil {
		t.Fatal(err)
	}
	// Simulates a correction arriving through sync, without an application marker.
	if _, err = f.db.Exec("UPDATE payments SET amount=4000,updated_at=? WHERE id=?", time.Now().UnixMilli(), payment); err != nil {
		t.Fatal(err)
	}
	report, err := uc.Report(ctx, reports.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil {
		t.Fatal(err)
	}
	if report.Session.ExpectedCash != 20 || report.Session.CurrentExpectedCash == nil || *report.Session.CurrentExpectedCash != 40 || report.Session.Difference != nil || *report.Session.CountedCash != 20 || *report.Session.CashLeft != 20 {
		t.Fatalf("history was overwritten: %+v", report.Session)
	}
	if _, err = uc.Reconcile(ctx, reports.CashReconcileInput{GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", SessionID: first.CashCloseID, CountedCash: 40}); err == nil {
		t.Fatal("historical count overwritten")
	}
}
