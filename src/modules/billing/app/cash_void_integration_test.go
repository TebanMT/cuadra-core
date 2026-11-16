//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	reports "github.com/cuadra/cuadra-core/src/application/reports"
	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type cashVoidFixture struct {
	*salesFixture
	cash   *reports.CashClose
	create *expApp.CreateCashMovement
	void   *expApp.VoidOwnCashMovement
}

func setupCashVoid(t *testing.T) *cashVoidFixture {
	t.Helper()
	f := setupSales(t)
	movements := expRepos.NewCashMovementSQLiteRepository()
	expenses := expRepos.NewExpenseSQLiteRepository()
	cash := countedPeriodUC(f).WithCashMovements(movements).WithExpenses(expenses)
	return &cashVoidFixture{
		salesFixture: f,
		cash:         cash,
		create:       expApp.NewCreateCashMovement(movements, f.uow, f.recorder, expApp.OperationalPolicy{}).WithExpenses(expenses),
		void:         expApp.NewVoidOwnCashMovement(movements, expenses, cash, f.uow, f.recorder),
	}
}

func (f *cashVoidFixture) seedOperator(t *testing.T, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC().UnixMilli()
	if _, err := f.db.Exec(`INSERT INTO users(id,gym_id,version,created_at,updated_at,email,password_hash,full_name,role,active)
		VALUES(?,?,1,?,?,?,'unused',?,'operator',1)`, id.String(), f.gymID.String(), now, now, id.String()+"@test.local", name); err != nil {
		t.Fatalf("seed operator: %v", err)
	}
	return id
}

func (f *cashVoidFixture) record(t *testing.T, actor uuid.UUID, in expApp.CashMovementInput) *cashDomain.CashMovement {
	t.Helper()
	in.GymID, in.ActorUserID = f.gymID, actor
	if in.MovementOn.IsZero() {
		in.MovementOn = cashSessionBusinessDay(t)
	}
	m, err := f.create.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("record movement: %v", err)
	}
	return m
}

func (f *cashVoidFixture) voidAs(actor uuid.UUID, m *cashDomain.CashMovement, version int, reason string) error {
	return f.void.Execute(context.Background(), expApp.VoidOwnCashMovementInput{
		GymID: f.gymID, ActorUserID: actor, MovementID: m.ID, ExpectedVersion: version, Reason: reason,
	})
}

func (f *cashVoidFixture) expectedNow(t *testing.T) float64 {
	t.Helper()
	report := cashDayReport(t, f.salesFixture, f.cash, cashSessionBusinessDay(t))
	if report.Session == nil {
		t.Fatal("report without session")
	}
	return report.Session.ExpectedCash
}

func TestCashVoid_OperatorTakesBackTheirTypoBeforeTheCount(t *testing.T) {
	f := setupCashVoid(t)
	ctx := context.Background()
	day := cashSessionBusinessDay(t)
	ricardo, ana := f.seedOperator(t, "Ricardo"), f.seedOperator(t, "Ana")
	session, err := f.cash.Open(ctx, reports.CashOpenInput{GymID: f.gymID, ActorUserID: ricardo, Date: day, OpeningCash: 600, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	// $500 typed instead of $50 for the water jugs.
	typo := f.record(t, ricardo, expApp.CashMovementInput{
		Amount: 500, MovementType: cashDomain.CashOut, Reason: "Garrafones",
		Purpose: cashDomain.AsExpense, Category: expenseDomain.CategoryOther, IdempotencyKey: "typo",
	})
	if got := f.expectedNow(t); got != 100 {
		t.Fatalf("expected with typo = %v", got)
	}

	if err := f.voidAs(ana, typo, typo.Version, "no es mío"); !errors.Is(err, expErrors.ErrVoidNotOwnMovement) {
		t.Fatalf("another operator voiding = %v", err)
	}
	if err := f.voidAs(ricardo, typo, typo.Version, "x"); !errors.Is(err, expErrors.ErrCorrectionReasonRequired) {
		t.Fatalf("void without reason = %v", err)
	}
	if err := f.voidAs(ricardo, typo, typo.Version-1, "Escribí 500 en vez de 50"); !errors.Is(err, expErrors.ErrVersionConflict) {
		t.Fatalf("void with stale version = %v", err)
	}
	if err := f.voidAs(ricardo, typo, typo.Version, "Escribí 500 en vez de 50"); err != nil {
		t.Fatalf("own void: %v", err)
	}

	report := cashDayReport(t, f.salesFixture, f.cash, day)
	if len(report.CashMovements) != 0 || len(report.Expenses) != 0 || report.Session.ExpectedCash != 600 {
		t.Fatalf("after void movements=%+v expenses=%+v expected=%v", report.CashMovements, report.Expenses, report.Session.ExpectedCash)
	}
	var liveExpenses, voidAudits int
	if err := f.db.Get(&liveExpenses, `SELECT COUNT(*) FROM expenses WHERE deleted_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Get(&voidAudits, `SELECT COUNT(*) FROM audit_log WHERE entity_type='cash_movements' AND entity_id=? AND action='void_own'`, typo.ID.String()); err != nil {
		t.Fatal(err)
	}
	if liveExpenses != 0 || voidAudits != 1 {
		t.Fatalf("live expenses=%d void audits=%d", liveExpenses, voidAudits)
	}
	if err := f.voidAs(ricardo, typo, typo.Version, "otra vez"); err == nil {
		t.Fatal("voiding twice must fail")
	}

	// The right amount is captured again and the count closes even.
	f.record(t, ricardo, expApp.CashMovementInput{
		Amount: 50, MovementType: cashDomain.CashOut, Reason: "Garrafones",
		Purpose: cashDomain.AsExpense, Category: expenseDomain.CategoryOther, IdempotencyKey: "fixed",
	})
	expected, counted := 550.0, 550.0
	closed, err := f.cash.Close(ctx, reports.CashCloseInput{
		GymID: f.gymID, ActorUserID: ricardo, Date: day, SessionID: session.ID,
		ExpectedCash: &expected, CountedCash: &counted,
	})
	if err != nil || closed.Discrepancy == nil || *closed.Discrepancy != 0 {
		t.Fatalf("close after fixing = %+v err=%v", closed, err)
	}
}

func TestCashVoid_AfterTheCountOnlyTheOwnerCorrects(t *testing.T) {
	f := setupCashVoid(t)
	ctx := context.Background()
	day := cashSessionBusinessDay(t)
	ricardo := f.seedOperator(t, "Ricardo")
	session, err := f.cash.Open(ctx, reports.CashOpenInput{GymID: f.gymID, ActorUserID: ricardo, Date: day, OpeningCash: 100, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	change := f.record(t, ricardo, expApp.CashMovementInput{
		Amount: 20, MovementType: cashDomain.CashIn, Reason: "Cambio", IdempotencyKey: "change",
	})
	expected, counted := 120.0, 120.0
	if _, err := f.cash.Close(ctx, reports.CashCloseInput{
		GymID: f.gymID, ActorUserID: ricardo, Date: day, SessionID: session.ID,
		ExpectedCash: &expected, CountedCash: &counted,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.voidAs(ricardo, change, change.Version, "Me equivoqué de caja"); !errors.Is(err, expErrors.ErrVoidCashSessionNotOpen) {
		t.Fatalf("void after the count = %v", err)
	}
	if report := cashDayReport(t, f.salesFixture, f.cash, day); len(report.CashMovements) != 1 || report.Session.Status == "stale" {
		t.Fatalf("a refused void must leave the count intact: %+v", report.Session)
	}
}

func TestCashVoid_WithoutAnOpenDrawerThereIsNothingToProtect(t *testing.T) {
	f := setupCashVoid(t)
	ricardo := f.seedOperator(t, "Ricardo")
	// A gym that never opens the drawer: the movement belongs to no session.
	handed := f.record(t, ricardo, expApp.CashMovementInput{
		Amount: 300, MovementType: cashDomain.CashOut, Reason: "Entrega al dueño",
		Purpose: cashDomain.NonOperating, IdempotencyKey: "handed",
	})
	err := f.voidAs(ricardo, handed, handed.Version, "Ya no se entregó")
	var custom sharedDomain.CustomError
	if !errors.Is(err, expErrors.ErrVoidCashSessionNotOpen) || !errors.As(err, &custom) || custom.ErrorCode != sharedDomain.CodeBusiness {
		t.Fatalf("void without session = %v", err)
	}
}
