//go:build sidecar

package app_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	billingRepos "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func registerCashProductSale(t *testing.T, fixture *salesFixture, productID uuid.UUID) uuid.UUID {
	t.Helper()
	out, err := fixture.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: fixture.gymID, ActorUserID: fixture.ownerID, Method: "cash",
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("register cash sale: %v", err)
	}
	return out.PaymentID
}

func cashSessionBusinessDay(t *testing.T) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/Mexico_City")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(loc)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

func seedCustomCashDrawer(t *testing.T, fixture *salesFixture, drawerID uuid.UUID, name string) *billingRepos.CashDrawerSQLiteRepository {
	t.Helper()
	repo := billingRepos.NewCashDrawerSQLiteRepository()
	now := time.Now().UTC()
	drawer := &cashCloseDomain.CashDrawer{ID: drawerID, GymID: fixture.gymID, Version: 1,
		Code: cashCloseDomain.DrawerCode(fixture.gymID, drawerID), Name: name, Active: true,
		CreatedAt: now, UpdatedAt: now}
	if err := fixture.uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := repo.Create(tx, drawer)
		return err
	}); err != nil {
		t.Fatalf("seed custom cash drawer: %v", err)
	}
	return repo
}

func TestCashSession_LateActivityBeforeWithdrawalRefreshesSameSequence(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	registerCashProductSale(t, f, productID)
	day := cashSessionBusinessDay(t)
	counted20 := 20.0
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	first, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, CountedCash: &counted20,
	})
	if err != nil {
		t.Fatalf("first close: %v", err)
	}

	registerCashProductSale(t, f, productID)
	report, err := uc.Report(context.Background(), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil {
		t.Fatalf("report late cash: %v", err)
	}
	if len(report.Sessions) != 1 || report.Session == nil || report.Session.Status != cashCloseDomain.StatusStale {
		t.Fatalf("sessions after late cash = %+v, want one effective stale sequence", report.Sessions)
	}
	if report.Session.ExpectedCash != 40 || report.Session.Difference != nil || report.RequiresNewSession {
		t.Fatalf("late pre-withdraw view = %+v", report.Session)
	}
	if _, err := uc.Withdraw(context.Background(), reportsApp.CashWithdrawInput{
		GymID: f.gymID, ActorUserID: f.ownerID, SessionID: first.CashCloseID,
		CashLeft: 0, Destination: cashCloseDomain.DestinationGymFund,
	}); err == nil {
		t.Fatal("withdrawal must require a fresh count after late activity")
	}
	tx, _ := f.uow.Query(context.Background())
	persisted, err := f.cashCloseEvents.GetByID(tx, f.gymID, first.CashCloseID)
	if err != nil || persisted == nil || persisted.Status != cashCloseDomain.StatusStale {
		t.Fatalf("withdrawal rejection must persist stale state: %+v err=%v", persisted, err)
	}

	counted40 := 40.0
	correctionReason := "ingreso posterior al primer conteo"
	refreshed, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, CountedCash: &counted40,
		CorrectionReason: &correctionReason,
	})
	if err != nil {
		t.Fatalf("refresh same session: %v", err)
	}
	if refreshed.CashCloseID != first.CashCloseID || refreshed.Session.Sequence != 1 || refreshed.CalculatedCash != 40 {
		t.Fatalf("refreshed = %+v; want same sequence 1 at $40", refreshed)
	}
}

func TestCashSession_SyncedCorrectionAfterCloseInvalidatesCountWithoutMarker(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	paymentID := registerCashProductSale(t, f, productID)
	day := cashSessionBusinessDay(t)
	counted := 20.0
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	closed, err := cash.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, CountedCash: &counted,
	})
	if err != nil || closed.Session == nil || closed.Session.ClosedAt == nil {
		t.Fatalf("close=%+v err=%v", closed, err)
	}

	// Emula que la corrección llega por sync antes que la mutación de la
	// sesión. La certificación debe caer al rederivar la actividad física, sin
	// depender del orden en que se proyecten las dos filas.
	updatedAt := closed.Session.ClosedAt.Add(time.Millisecond).UnixMilli()
	if _, err = f.db.Exec(`UPDATE payments SET amount=?,recognized_amount=?,version=version+1,updated_at=? WHERE id=?`,
		4000, 4000, updatedAt, paymentID.String()); err != nil {
		t.Fatal(err)
	}
	report, err := cash.Report(context.Background(), reportsApp.CashCloseReportInput{
		GymID: f.gymID, Date: day,
	})
	if err != nil || report.Session == nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if report.Session.Status != cashCloseDomain.StatusStale || !report.Session.IsStale ||
		report.Session.ExpectedCash != 40 || report.Session.CountedCash == nil ||
		*report.Session.CountedCash != 20 || report.Session.Difference != nil {
		t.Fatalf("synced correction must invalidate the old count: %+v", report.Session)
	}
}

func TestCashSession_SyncedCorrectionAfterWithdrawalPreservesTransferButVoidsDifference(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	paymentID := registerCashProductSale(t, f, productID)
	day := cashSessionBusinessDay(t)
	counted, left := 20.0, 0.0
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	closed, err := cash.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, CountedCash: &counted,
		Withdraw: true, CashLeft: &left,
	})
	if err != nil || closed.Session == nil || closed.Session.WithdrawnAt == nil {
		t.Fatalf("withdrawn close=%+v err=%v", closed, err)
	}
	updatedAt := closed.Session.WithdrawnAt.Add(time.Millisecond).UnixMilli()
	if _, err = f.db.Exec(`UPDATE payments SET amount=?,recognized_amount=?,version=version+1,updated_at=? WHERE id=?`,
		4000, 4000, updatedAt, paymentID.String()); err != nil {
		t.Fatal(err)
	}

	report, err := cash.Report(context.Background(), reportsApp.CashCloseReportInput{
		GymID: f.gymID, Date: day,
	})
	if err != nil || report.Session == nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if report.Session.Status != cashCloseDomain.StatusWithdrawn ||
		!report.Session.AdjustedAfterWithdrawal || report.Session.Difference != nil ||
		report.Session.ExpectedCash != 20 || report.Session.CurrentExpectedCash == nil || *report.Session.CurrentExpectedCash != 40 || report.Session.WithdrawnCash == nil ||
		*report.Session.WithdrawnCash != 20 || report.RequiresNewSession {
		t.Fatalf("post-withdrawal correction must retain the transfer but invalidate its difference: %+v", report.Session)
	}
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := f.cashCloseReader.SessionCoverage(tx, billingRepo.CashSessionCoverageQuery{
		GymID: f.gymID, From: day, To: day,
	})
	if err != nil || coverage.Complete || coverage.UncoveredActivityDays != 1 ||
		coverage.AdjustedAfterWithdrawalSessions != 1 {
		t.Fatalf("post-withdrawal correction coverage=%+v err=%v", coverage, err)
	}
}

func TestCashSession_CorrectingCertifiedCountRequiresOwnerAndReason(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 2)
	registerCashProductSale(t, f, productID)
	counted := 20.0
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	closed, err := cash.Close(context.Background(), reportsApp.CashCloseInput{GymID: f.gymID,
		ActorUserID: f.ownerID, Date: cashSessionBusinessDay(t), CountedCash: &counted})
	if err != nil {
		t.Fatal(err)
	}
	discrepancyReason := "faltó un peso"
	correctionReason := "el conteo original se capturó mal"
	input := reportsApp.CashReconcileInput{GymID: f.gymID, ActorUserID: f.ownerID,
		ActorRole: "operator", SessionID: closed.CashCloseID, CountedCash: 19,
		DiscrepancyReason: &discrepancyReason, CorrectionReason: &correctionReason}
	if _, err := cash.Reconcile(context.Background(), input); !errors.Is(err, cashCloseDomain.ErrCorrectionOwner) {
		t.Fatalf("operator certified recount error=%v", err)
	}
	input.ActorRole = "owner"
	input.CorrectionReason = nil
	if _, err := cash.Reconcile(context.Background(), input); !errors.Is(err, cashCloseDomain.ErrCorrectionReason) {
		t.Fatalf("owner recount without reason error=%v", err)
	}
	input.CorrectionReason = &correctionReason
	view, err := cash.Reconcile(context.Background(), input)
	if err != nil || view.CountedCash == nil || *view.CountedCash != 19 ||
		view.Difference == nil || *view.Difference != -1 || view.CorrectionReason == nil ||
		*view.CorrectionReason != correctionReason {
		t.Fatalf("corrected certified count=%+v err=%v", view, err)
	}
	var audited int
	if err := f.db.Get(&audited, `SELECT COUNT(*) FROM audit_log WHERE entity_type='cash_close_events'
		AND entity_id=? AND changes LIKE '%previous_counted_cash%' AND changes LIKE '%correction_reason%'`,
		closed.CashCloseID.String()); err != nil || audited != 1 {
		t.Fatalf("certified recount audit=%d err=%v", audited, err)
	}
	if _, err := cash.Withdraw(context.Background(), reportsApp.CashWithdrawInput{GymID: f.gymID,
		ActorUserID: f.ownerID, SessionID: closed.CashCloseID, CashLeft: 0,
		Destination: cashCloseDomain.DestinationGymFund}); err != nil {
		t.Fatal(err)
	}
	if _, err := cash.Reconcile(context.Background(), input); err == nil {
		t.Fatal("withdrawn count must remain immutable")
	}
}

func TestCashSession_LateActivityAfterWithdrawalRequiresAndCreatesNextSequence(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	registerCashProductSale(t, f, productID)
	day := cashSessionBusinessDay(t)
	counted20, left := 20.0, 5.0
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	first, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, CountedCash: &counted20,
		Withdraw: true, CashLeft: &left,
	})
	if err != nil {
		t.Fatalf("close and withdraw: %v", err)
	}
	if first.Session.Status != cashCloseDomain.StatusWithdrawn || first.Session.WithdrawnCash == nil || *first.Session.WithdrawnCash != 15 {
		t.Fatalf("withdrawn session = %+v", first.Session)
	}

	// SQLite timestamps have millisecond precision. Cross the storage tick so
	// the physical event is unambiguously later than the recorded withdrawal.
	time.Sleep(2 * time.Millisecond)
	registerCashProductSale(t, f, productID)
	report, err := uc.Report(context.Background(), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil {
		t.Fatalf("report uncovered cash: %v", err)
	}
	if len(report.Sessions) != 1 || report.Session.Status != cashCloseDomain.StatusWithdrawn ||
		!report.RequiresNewSession || report.UncoveredCashActivity != 20 {
		t.Fatalf("post-withdraw report = %+v", report)
	}

	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := f.cashCloseReader.SessionCoverage(tx, billingRepo.CashSessionCoverageQuery{
		GymID: f.gymID, From: day, To: day,
	})
	if err != nil {
		t.Fatalf("coverage before second close: %v", err)
	}
	if coverage.Complete || coverage.UncoveredActivityDays != 1 {
		t.Fatalf("coverage = %+v, want uncovered/incomplete", coverage)
	}

	counted25 := 25.0 // $5 left in the drawer + $20 late activity.
	second, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, CountedCash: &counted25,
	})
	if err != nil {
		t.Fatalf("second sequence close: %v", err)
	}
	if second.CashCloseID == first.CashCloseID || second.Session.Sequence != 2 ||
		second.Session.OpeningCash != 5 || second.Session.ActivityCash != 20 || second.CalculatedCash != 25 {
		t.Fatalf("second sequence = %+v", second)
	}

	tx, _ = f.uow.Query(context.Background())
	latest, err := f.cashCloseEvents.GetByDate(tx, f.gymID, day)
	if err != nil || latest == nil || latest.Sequence != 2 || latest.ID != second.CashCloseID {
		t.Fatalf("GetByDate latest = %+v err=%v", latest, err)
	}
	coverage, err = f.cashCloseReader.SessionCoverage(tx, billingRepo.CashSessionCoverageQuery{
		GymID: f.gymID, From: day, To: day,
	})
	if err != nil || !coverage.Complete || coverage.UncoveredActivityDays != 0 {
		t.Fatalf("coverage after second close = %+v err=%v", coverage, err)
	}
}

func TestCashSession_TwoDrawersAreAttributedSelectedAndClosedIndependently(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	customDrawerID := uuid.New()
	drawers := seedCustomCashDrawer(t, f, customDrawerID, "Recepción norte")
	registerCashProductSale(t, f, productID)
	if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash",
		CashDrawerID: &customDrawerID,
		Items:        []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
	}); err != nil {
		t.Fatalf("register custom-drawer sale: %v", err)
	}
	if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "transfer",
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
	}); err != nil {
		t.Fatalf("register gym-wide transfer sale: %v", err)
	}
	day := cashSessionBusinessDay(t)
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithCashDrawers(drawers)
	report, err := cash.Report(context.Background(), reportsApp.CashCloseReportInput{
		GymID: f.gymID, Date: day, DrawerID: &customDrawerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.SelectedDrawerID != customDrawerID || len(report.Drawers) != 2 ||
		!report.RequiresNewSession || report.UncoveredCashActivity != 20 {
		t.Fatalf("custom drawer discovery/report=%+v", report)
	}
	if report.Totals.ByMethod["cash"] != 20 || report.Totals.ByMethod["transfer"] != 20 ||
		report.Totals.GrandTotal != 40 {
		t.Fatalf("custom drawer totals=%+v grand=%v; want local cash 20 + global transfer 20",
			report.Totals.ByMethod, report.Totals.GrandTotal)
	}
	activity := map[uuid.UUID]float64{}
	for _, drawer := range report.Drawers {
		activity[drawer.ID] = drawer.ActivityCash
	}
	if activity[f.gymID] != 20 || activity[customDrawerID] != 20 {
		t.Fatalf("drawer activities=%v", activity)
	}
	counted := 20.0
	mainClose, err := cash.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, CountedCash: &counted,
	})
	if err != nil {
		t.Fatalf("close main: %v", err)
	}
	customClose, err := cash.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day,
		DrawerID: &customDrawerID, CountedCash: &counted,
	})
	if err != nil {
		t.Fatalf("close custom: %v", err)
	}
	if customClose.CashCloseID == mainClose.CashCloseID || customClose.Session.DrawerID != customDrawerID ||
		customClose.Session.DrawerCode == cashCloseDomain.DefaultDrawerCode || customClose.CalculatedCash != 20 {
		t.Fatalf("independent custom session=%+v main=%+v", customClose.Session, mainClose.Session)
	}
	report, err = cash.Report(context.Background(), reportsApp.CashCloseReportInput{
		GymID: f.gymID, Date: day, DrawerID: &customDrawerID,
	})
	if err != nil || report.Session == nil || report.Session.ID != customClose.CashCloseID ||
		len(report.Sessions) != 1 || report.RequiresNewSession {
		t.Fatalf("selected custom session=%+v sessions=%+v err=%v", report.Session, report.Sessions, err)
	}
	tx, _ := f.uow.Query(context.Background())
	coverage, err := f.cashCloseReader.SessionCoverage(tx, billingRepo.CashSessionCoverageQuery{GymID: f.gymID, From: day, To: day})
	if err != nil || !coverage.Complete || coverage.ActiveSessions != 2 || coverage.UncoveredActivityDays != 0 {
		t.Fatalf("two-drawer coverage=%+v err=%v", coverage, err)
	}
}

func TestCashSession_SelectedDrawerScopesIncomeExpensesMovementsAndSessions(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	customDrawerID := uuid.New()
	drawers := seedCustomCashDrawer(t, f, customDrawerID, "Recepción norte")
	day := cashSessionBusinessDay(t)
	if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", PaymentDate: day,
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registerSale().Execute(context.Background(), billingApp.RegisterSaleInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Method: "cash", CashDrawerID: &customDrawerID, PaymentDate: day,
		Items: []billingApp.SaleLineInput{{ProductID: productID, Quantity: 2}},
	}); err != nil {
		t.Fatal(err)
	}

	movements := expRepos.NewCashMovementSQLiteRepository()
	expenses := expRepos.NewExpenseSQLiteRepository()
	createMovement := expApp.NewCreateCashMovement(movements, f.uow, f.recorder, expApp.OperationalPolicy{})
	classify := expApp.NewClassifyCashMovement(movements, expenses, f.uow, f.recorder, expApp.OperationalPolicy{})
	for _, item := range []struct {
		amount   float64
		drawerID *uuid.UUID
		key      string
	}{
		{amount: 5, key: "main-cleaning"},
		{amount: 7, drawerID: &customDrawerID, key: "north-cleaning"},
	} {
		movement, err := createMovement.Execute(context.Background(), expApp.CashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day, Amount: item.amount,
			MovementType: cashDomain.CashOut, Reason: "Limpieza", CashDrawerID: item.drawerID,
			IdempotencyKey: item.key,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := classify.Execute(context.Background(), expApp.ClassifyCashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementID: movement.ID, PaidOn: day,
			Category: expenseDomain.CategoryOther, Classification: expenseDomain.ClassificationVariable,
		}); err != nil {
			t.Fatal(err)
		}
	}

	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithCashDrawers(drawers).WithCashMovements(movements).WithExpenses(expenses)
	mainCount, customCount := 15.0, 33.0
	if _, err := cash.Close(context.Background(), reportsApp.CashCloseInput{GymID: f.gymID,
		ActorUserID: f.ownerID, Date: day, CountedCash: &mainCount}); err != nil {
		t.Fatal(err)
	}
	if _, err := cash.Close(context.Background(), reportsApp.CashCloseInput{GymID: f.gymID,
		ActorUserID: f.ownerID, Date: day, DrawerID: &customDrawerID, CountedCash: &customCount}); err != nil {
		t.Fatal(err)
	}

	assertDrawer := func(drawerID uuid.UUID, income, outflow, net float64) {
		t.Helper()
		report, err := cash.Report(context.Background(), reportsApp.CashCloseReportInput{
			GymID: f.gymID, Date: day, DrawerID: &drawerID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if report.Totals.GrandTotal != income || report.Totals.ByMethod["cash"] != income ||
			report.ExpensesTotal != outflow || report.CashOutTotal != outflow || report.NetTotal != net ||
			len(report.Expenses) != 1 || len(report.CashMovements) != 1 || len(report.Sessions) != 1 ||
			report.Session == nil || report.Session.DrawerID != drawerID || report.Session.ExpectedCash != net {
			t.Fatalf("drawer %s scoped report=%+v totals=%+v sessions=%+v", drawerID, report, report.Totals, report.Sessions)
		}
		for _, movement := range report.CashMovements {
			if movement.CashDrawerID != drawerID {
				t.Fatalf("foreign movement leaked into drawer %s: %+v", drawerID, movement)
			}
		}
	}
	assertDrawer(f.gymID, 20, 5, 15)
	assertDrawer(customDrawerID, 40, 7, 33)
}

func TestCashSession_MovingCertifiedMovementBetweenDrawersInvalidatesBoth(t *testing.T) {
	f := setupSales(t)
	movements := expRepos.NewCashMovementSQLiteRepository()
	day := cashSessionBusinessDay(t)
	customDrawerID := uuid.New()
	drawers := seedCustomCashDrawer(t, f, customDrawerID, "Recepción secundaria")
	movement, err := expApp.NewCreateCashMovement(movements, f.uow, f.recorder, expApp.OperationalPolicy{}).
		Execute(context.Background(), expApp.CashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day,
			Amount: 20, MovementType: cashDomain.CashIn, Reason: "Cambio",
			IdempotencyKey: "move-between-drawers",
		})
	if err != nil {
		t.Fatal(err)
	}
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithCashMovements(movements).WithCashDrawers(drawers)
	countedMain, countedCustom := 20.0, 0.0
	if _, err = cash.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, CountedCash: &countedMain,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = cash.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day,
		DrawerID: &customDrawerID, CountedCash: &countedCustom,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = expApp.NewUpdateCashMovement(movements, f.uow, f.recorder, expApp.OperationalPolicy{}).
		WithCashSessionMarker(cash).
		Execute(context.Background(), movement.ID, expApp.CashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day,
			Amount: 20, MovementType: cashDomain.CashIn, Reason: "Cambio",
			CashDrawerID: &customDrawerID, ExpectedVersion: movement.Version,
			CorrectionReason: "se registró en el cajón equivocado",
		}); err != nil {
		t.Fatalf("move certified movement: %v", err)
	}
	for _, drawerID := range []uuid.UUID{f.gymID, customDrawerID} {
		report, reportErr := cash.Report(context.Background(), reportsApp.CashCloseReportInput{
			GymID: f.gymID, Date: day, DrawerID: &drawerID,
		})
		if reportErr != nil || report.Session == nil || report.SelectedDrawerID != drawerID ||
			report.Session.Status != cashCloseDomain.StatusStale || report.Session.Difference != nil {
			t.Fatalf("drawer %s after reassignment: session=%+v report=%+v err=%v", drawerID, report.Session, report, reportErr)
		}
	}
}

func TestCashSession_SQLiteRejectsMalformedLifecycleFromDirectWriter(t *testing.T) {
	f := setupSales(t)
	now := time.Now().UTC().UnixMilli()
	_, err := f.db.Exec(`INSERT INTO cash_close_events(
		id,gym_id,version,created_at,updated_at,close_date,calculated_cash,closed_by)
		VALUES(?,?,1,?,?,?,?,?)`, uuid.New(), f.gymID, now, now, time.Now().UTC().Format("2006-01-02"), 0, f.ownerID)
	if err == nil {
		t.Fatal("direct/sync-style insert without required session lifecycle must be rejected")
	}
}

func TestCashSession_SQLiteLifecycleConstraintsRejectInvalidUpdates(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 2)
	registerCashProductSale(t, f, productID)
	counted, left := 20.0, 0.0
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	closed, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: cashSessionBusinessDay(t),
		CountedCash: &counted, CashLeft: &left, Withdraw: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"invalid status":      `UPDATE cash_close_events SET status='invented' WHERE id=?`,
		"invalid sequence":    `UPDATE cash_close_events SET sequence=0 WHERE id=?`,
		"invalid destination": `UPDATE cash_close_events SET withdrawal_destination='external' WHERE id=?`,
		"invalid math":        `UPDATE cash_close_events SET withdrawn_cash=1 WHERE id=?`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.db.Exec(statement, closed.CashCloseID.String()); err == nil {
				t.Fatalf("direct/sync-style %s must be rejected", name)
			}
		})
	}
}

func TestCashSession_RecordOnlyCorrectionAfterWithdrawalPreservesTransferAndInvalidatesDifference(t *testing.T) {
	f := setupSales(t)
	productID := f.seedProduct(t, "Agua", 20, 10)
	paymentID := registerCashProductSale(t, f, productID)
	day := cashSessionBusinessDay(t)
	counted, left := 20.0, 0.0
	uc := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder)
	closed, err := uc.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, CountedCash: &counted,
		Withdraw: true, CashLeft: &left,
	})
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	reason := "se capturaron 10 piezas en vez de 2"
	err = f.uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		payment, err := f.paymentRepo.GetByID(tx, paymentID)
		if err != nil {
			return err
		}
		stx := tx.(*sharedDomain.SqlxTransaction)
		if _, err := stx.Exec(context.Background(), `UPDATE payments
			SET amount=?,version=version+1,updated_at=? WHERE id=?`,
			400, time.Now().UTC().UnixMilli(), paymentID.String()); err != nil {
			return err
		}
		return uc.MarkRecordCorrection(context.Background(), tx, billingApp.CashSessionAdjustmentInput{
			GymID: f.gymID, ActorUserID: f.ownerID, OperationalDate: day,
			OriginalRecordedAt: payment.CreatedAt, Reason: reason,
		})
	})
	if err != nil {
		t.Fatalf("record-only correction: %v", err)
	}

	tx, _ := f.uow.Query(context.Background())
	session, err := f.cashCloseEvents.GetByID(tx, f.gymID, closed.CashCloseID)
	if err != nil || session == nil {
		t.Fatalf("read adjusted session: %+v %v", session, err)
	}
	if session.Status != cashCloseDomain.StatusWithdrawn || !session.AdjustedAfterWithdrawal ||
		session.WithdrawnCash == nil || *session.WithdrawnCash != 20 || session.Difference() != nil {
		t.Fatalf("adjusted withdrawn session = %+v", session)
	}
	var storedDiscrepancy sql.NullInt64
	if err = f.db.Get(&storedDiscrepancy, `SELECT discrepancy FROM cash_close_events WHERE id=?`, closed.CashCloseID); err != nil || storedDiscrepancy.Valid {
		t.Fatalf("SQLite discrepancy must be NULL after post-withdraw adjustment: value=%v err=%v", storedDiscrepancy, err)
	}
	coverage, err := f.cashCloseReader.SessionCoverage(tx, billingRepo.CashSessionCoverageQuery{
		GymID: f.gymID, From: day, To: day,
	})
	if err != nil || coverage.Complete || coverage.AdjustedAfterWithdrawalSessions != 1 {
		t.Fatalf("coverage after historical correction = %+v err=%v", coverage, err)
	}
	report, err := uc.Report(context.Background(), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil || report.Session == nil || report.Session.Difference != nil ||
		!report.Session.AdjustedAfterWithdrawal || report.RequiresNewSession {
		t.Fatalf("adjusted report = %+v err=%v", report.Session, err)
	}
}

func TestCashSession_EditingCertifiedCashExpenseRequiresReasonAndMarksStale(t *testing.T) {
	f := setupSales(t)
	movements := expRepos.NewCashMovementSQLiteRepository()
	expenses := expRepos.NewExpenseSQLiteRepository()
	policy := expApp.OperationalPolicy{}
	day := cashSessionBusinessDay(t)
	description := "Limpieza"
	created, err := expApp.NewCreateExpense(expenses, f.uow, f.recorder).
		WithOperational(movements, policy).
		Execute(context.Background(), expApp.CreateExpenseInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day,
			Amount: 20, Category: expenseDomain.CategoryOther, Description: &description,
			PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister,
			Classification: expenseDomain.ClassificationVariable,
		})
	if err != nil {
		t.Fatalf("create cash expense: %v", err)
	}
	opening, counted := 100.0, 80.0
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithExpenses(expenses).WithCashMovements(movements)
	closed, err := cash.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day,
		OpeningCash: &opening, CountedCash: &counted,
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	update := expApp.NewUpdateExpense(expenses, f.uow, f.recorder).
		WithOperational(movements, policy).WithCashSessionMarker(cash)
	input := expApp.UpdateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: created.ExpenseID,
		ExpenseDate: day, Amount: 30, Category: expenseDomain.CategoryOther,
		Description: &description, PaymentMethod: expenseDomain.PaymentCash,
		PaidFrom:       expenseDomain.PaidFromCashRegister,
		Classification: expenseDomain.ClassificationVariable, ExpectedVersion: 1,
	}
	if err = update.Execute(context.Background(), input); !errors.Is(err, expErrors.ErrCorrectionReasonRequired) {
		t.Fatalf("certified edit without reason error=%v", err)
	}
	input.CorrectionReason = "el gasto real fue de 30"
	if err = update.Execute(context.Background(), input); err != nil {
		t.Fatalf("certified edit with reason: %v", err)
	}
	report, err := cash.Report(context.Background(), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil || report.Session == nil {
		t.Fatalf("report: %+v err=%v", report, err)
	}
	if report.Session.ID != closed.CashCloseID || report.Session.Status != cashCloseDomain.StatusStale ||
		report.Session.ExpectedCash != 70 || report.Session.Difference != nil {
		t.Fatalf("stale corrected session=%+v", report.Session)
	}
}

func TestCashSession_DeletingCashExpenseAfterWithdrawalPreservesTransfer(t *testing.T) {
	f := setupSales(t)
	movements := expRepos.NewCashMovementSQLiteRepository()
	expenses := expRepos.NewExpenseSQLiteRepository()
	policy := expApp.OperationalPolicy{}
	day := cashSessionBusinessDay(t)
	created, err := expApp.NewCreateExpense(expenses, f.uow, f.recorder).
		WithOperational(movements, policy).
		Execute(context.Background(), expApp.CreateExpenseInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day,
			Amount: 20, Category: expenseDomain.CategoryOther,
			PaymentMethod: expenseDomain.PaymentCash, PaidFrom: expenseDomain.PaidFromCashRegister,
		})
	if err != nil {
		t.Fatal(err)
	}
	opening, counted, left := 100.0, 80.0, 10.0
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithExpenses(expenses).WithCashMovements(movements)
	closed, err := cash.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, OpeningCash: &opening,
		CountedCash: &counted, CashLeft: &left, Withdraw: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	del := expApp.NewDeleteExpense(expenses, f.uow, f.recorder).
		WithOperational(movements, policy).WithCashSessionMarker(cash)
	if err = del.Execute(context.Background(), expApp.DeleteExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: created.ExpenseID,
		ExpectedVersion: 1, CorrectionReason: "el pago nunca salió de la caja",
	}); err != nil {
		t.Fatalf("delete adjusted cash expense: %v", err)
	}
	tx, _ := f.uow.Query(context.Background())
	session, err := f.cashCloseEvents.GetByID(tx, f.gymID, closed.CashCloseID)
	if err != nil || session == nil || !session.AdjustedAfterWithdrawal ||
		session.WithdrawnCash == nil || *session.WithdrawnCash != 70 || session.Difference() != nil {
		t.Fatalf("adjusted session=%+v err=%v", session, err)
	}
	var transfers int
	if err = f.db.Get(&transfers, `SELECT COUNT(*) FROM cash_transfers WHERE session_id=? AND amount=7000`, closed.CashCloseID); err != nil || transfers != 1 {
		t.Fatalf("historical transfer changed: count=%d err=%v", transfers, err)
	}
}

func TestCashSession_FundExpenseAfterCloseDoesNotTouchDrawer(t *testing.T) {
	f := setupSales(t)
	movements := expRepos.NewCashMovementSQLiteRepository()
	expenses := expRepos.NewExpenseSQLiteRepository()
	day := cashSessionBusinessDay(t)
	opening, counted := 100.0, 100.0
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithExpenses(expenses).WithCashMovements(movements)
	if _, err := cash.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day,
		OpeningCash: &opening, CountedCash: &counted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := expApp.NewCreateExpense(expenses, f.uow, f.recorder).
		WithOperational(movements, expApp.OperationalPolicy{}).
		Execute(context.Background(), expApp.CreateExpenseInput{
			GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day,
			Amount: 1500, Category: expenseDomain.CategoryRent,
			PaymentMethod: expenseDomain.PaymentTransfer, PaidFrom: expenseDomain.PaidFromGymFund,
		}); err != nil {
		t.Fatalf("fund rent after close: %v", err)
	}
	report, err := cash.Report(context.Background(), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil || report.Session == nil || report.Session.Status != cashCloseDomain.StatusReconciled ||
		report.Session.ExpectedCash != 100 || report.RequiresNewSession || len(report.CashMovements) != 0 {
		t.Fatalf("fund expense changed drawer: report=%+v session=%+v err=%v", report, report.Session, err)
	}
}

func TestCashSession_ClassifyingExistingOutflowAfterWithdrawalIsCashNeutral(t *testing.T) {
	f := setupSales(t)
	movements := expRepos.NewCashMovementSQLiteRepository()
	expenses := expRepos.NewExpenseSQLiteRepository()
	policy := expApp.OperationalPolicy{}
	day := cashSessionBusinessDay(t)
	movement, err := expApp.NewCreateCashMovement(movements, f.uow, f.recorder, policy).
		Execute(context.Background(), expApp.CashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day,
			Amount: 20, MovementType: cashDomain.CashOut, Reason: "Compra de limpieza",
		})
	if err != nil {
		t.Fatal(err)
	}
	opening, counted, left := 100.0, 80.0, 10.0
	cash := reportsApp.NewCashClose(f.cashCloseReader, f.cashCloseEvents, f.uow, f.recorder).
		WithExpenses(expenses).WithCashMovements(movements)
	closed, err := cash.Close(context.Background(), reportsApp.CashCloseInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Date: day, OpeningCash: &opening,
		CountedCash: &counted, CashLeft: &left, Withdraw: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = expApp.NewClassifyCashMovement(movements, expenses, f.uow, f.recorder, policy).
		Execute(context.Background(), expApp.ClassifyCashMovementInput{
			GymID: f.gymID, ActorUserID: f.ownerID, MovementID: movement.ID,
			PaidOn: day, Category: expenseDomain.CategoryOther,
			Classification: expenseDomain.ClassificationVariable,
		}); err != nil {
		t.Fatalf("classify after withdrawal: %v", err)
	}
	report, err := cash.Report(context.Background(), reportsApp.CashCloseReportInput{GymID: f.gymID, Date: day})
	if err != nil || report.Session == nil || report.Session.ID != closed.CashCloseID ||
		report.Session.Status != cashCloseDomain.StatusWithdrawn || report.Session.AdjustedAfterWithdrawal ||
		report.RequiresNewSession || report.Session.WithdrawnCash == nil || *report.Session.WithdrawnCash != 70 {
		t.Fatalf("classification changed historical cash: session=%+v report=%+v err=%v", report.Session, report, err)
	}
}
