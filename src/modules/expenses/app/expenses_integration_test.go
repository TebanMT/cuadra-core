//go:build sidecar

package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepoLite "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

type expensesFixture struct {
	t           *testing.T
	db          *sqlx.DB
	uow         sharedDomain.UnitOfWork
	recorder    audit.Recorder
	gymID       uuid.UUID
	ownerID     uuid.UUID
	expenseRepo *expRepoLite.ExpenseSQLiteRepository
}

func setupExpenses(t *testing.T) *expensesFixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	dsn := dbPath + "?_foreign_keys=on"
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)

	for _, m := range []string{
		"../../../../db_migrations/sqlite/001_init_schema.sql",
		"../../../../db_migrations/sqlite/005_users_pin.sql",
		"../../../../db_migrations/sqlite/008_gym_charge_settings.sql",
		"../../../../db_migrations/sqlite/012_expenses.sql",
	} {
		schema, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(schema)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}
	uow := sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue())
	recorder := audit.NewSQLiteRecorder()

	signup := usersApp.NewSignupOwner(
		usersRepoLite.NewUserSQLiteRepository(),
		gymRepoLite.NewGymSQLiteRepository(),
		uow,
		auth.NewJWTService("test-secret"),
		recorder,
		30,
	)
	owner, err := signup.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Owner",
		Email:           "owner@gym.com",
		Password:        "supersecret123",
		PasswordConfirm: "supersecret123",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}

	return &expensesFixture{
		t:           t,
		db:          db,
		uow:         uow,
		recorder:    recorder,
		gymID:       owner.GymID,
		ownerID:     owner.UserID,
		expenseRepo: expRepoLite.NewExpenseSQLiteRepository(),
	}
}

func (f *expensesFixture) createUC() *expApp.CreateExpense {
	return expApp.NewCreateExpense(f.expenseRepo, f.uow, f.recorder)
}

func (f *expensesFixture) listUC() *expApp.ListExpenses {
	return expApp.NewListExpenses(f.expenseRepo, f.uow)
}

// ---------------------------------------------------------------------------
// CreateExpense
// ---------------------------------------------------------------------------

func TestCreateExpense_PersistsRow(t *testing.T) {
	f := setupExpenses(t)
	date, _ := time.Parse("2006-01-02", "2026-05-10")
	desc := "Luz mayo"
	out, err := f.createUC().Execute(context.Background(), expApp.CreateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ExpenseDate:   date,
		Amount:        1234.56,
		Category:      expenseDomain.CategoryUtilities,
		Description:   &desc,
		PaymentMethod: expenseDomain.PaymentTransfer,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var n int
	if err := f.db.Get(&n, "SELECT COUNT(*) FROM expenses WHERE id=?", out.ExpenseID.String()); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
	// Amount se guarda en cents en sqlite.
	var cents int64
	_ = f.db.Get(&cents, "SELECT amount FROM expenses WHERE id=?", out.ExpenseID.String())
	if cents != 123456 {
		t.Errorf("amount cents = %d, want 123456", cents)
	}
}

func TestCreateExpense_RejectsInvalidAmount(t *testing.T) {
	f := setupExpenses(t)
	date, _ := time.Parse("2006-01-02", "2026-05-10")
	_, err := f.createUC().Execute(context.Background(), expApp.CreateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ExpenseDate:   date,
		Amount:        0,
		Category:      expenseDomain.CategoryRent,
		PaymentMethod: expenseDomain.PaymentCash,
	})
	if err == nil {
		t.Errorf("expected validation error for zero amount")
	}
	if err != nil && !errors.Is(err, expErrors.ErrInvalidAmount) {
		// El wrapper CustomError preserva el sentinel via Unwrap.
		t.Logf("error: %v", err)
	}
}

func TestCreateExpense_RejectsInvalidCategory(t *testing.T) {
	f := setupExpenses(t)
	date, _ := time.Parse("2006-01-02", "2026-05-10")
	_, err := f.createUC().Execute(context.Background(), expApp.CreateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ExpenseDate:   date,
		Amount:        100,
		Category:      "no_existe",
		PaymentMethod: expenseDomain.PaymentCash,
	})
	if err == nil {
		t.Errorf("expected validation error for bad category")
	}
}

func TestCreateExpense_RejectsInvalidPaymentMethod(t *testing.T) {
	f := setupExpenses(t)
	date, _ := time.Parse("2006-01-02", "2026-05-10")
	_, err := f.createUC().Execute(context.Background(), expApp.CreateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ExpenseDate:   date,
		Amount:        100,
		Category:      expenseDomain.CategoryRent,
		PaymentMethod: "crypto",
	})
	if err == nil {
		t.Errorf("expected validation error for bad payment method")
	}
}

// ---------------------------------------------------------------------------
// ListExpenses + Aggregates
// ---------------------------------------------------------------------------

func TestListExpenses_FiltersAndAggregates(t *testing.T) {
	f := setupExpenses(t)
	createUC := f.createUC()
	d1, _ := time.Parse("2006-01-02", "2026-05-01")
	d2, _ := time.Parse("2026-01-02", "2026-05-10")
	_ = d2
	d3, _ := time.Parse("2006-01-02", "2026-05-10")

	// Renta cash 5000
	_, _ = createUC.Execute(context.Background(), expApp.CreateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ExpenseDate: d1, Amount: 5000, Category: expenseDomain.CategoryRent, PaymentMethod: expenseDomain.PaymentCash,
	})
	// Servicios transfer 1500
	_, _ = createUC.Execute(context.Background(), expApp.CreateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ExpenseDate: d3, Amount: 1500, Category: expenseDomain.CategoryUtilities, PaymentMethod: expenseDomain.PaymentTransfer,
	})
	// Sueldos cash 8000 — categoría dominante
	_, _ = createUC.Execute(context.Background(), expApp.CreateExpenseInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ExpenseDate: d3, Amount: 8000, Category: expenseDomain.CategorySalaries, PaymentMethod: expenseDomain.PaymentCash,
	})

	out, err := f.listUC().Execute(context.Background(), expApp.ListExpensesInput{GymID: f.gymID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out.Total != 3 {
		t.Errorf("total = %d, want 3", out.Total)
	}
	if got := out.Aggregates.Total; got != 14500 {
		t.Errorf("aggregates.Total = %v, want 14500", got)
	}
	if got := out.Aggregates.CashTotal; got != 13000 {
		t.Errorf("aggregates.CashTotal = %v, want 13000", got)
	}
	if got := out.Aggregates.NonCashTotal; got != 1500 {
		t.Errorf("aggregates.NonCashTotal = %v, want 1500", got)
	}
	if out.Aggregates.DominantCategory != expenseDomain.CategorySalaries {
		t.Errorf("dominant category = %q, want %q", out.Aggregates.DominantCategory, expenseDomain.CategorySalaries)
	}

	// Filtro categoría=renta
	rent, _ := f.listUC().Execute(context.Background(), expApp.ListExpensesInput{
		GymID: f.gymID, Category: expenseDomain.CategoryRent,
	})
	if rent.Total != 1 {
		t.Errorf("rent total = %d, want 1", rent.Total)
	}

	// Filtro rango fechas — solo d3 (10/05) en adelante.
	from := d3
	to := d3
	d10, _ := f.listUC().Execute(context.Background(), expApp.ListExpensesInput{
		GymID: f.gymID, From: &from, To: &to,
	})
	if d10.Total != 2 {
		t.Errorf("date-filtered total = %d, want 2", d10.Total)
	}

	// Filtro payment_method=cash
	cash, _ := f.listUC().Execute(context.Background(), expApp.ListExpensesInput{
		GymID: f.gymID, PaymentMethod: expenseDomain.PaymentCash,
	})
	if cash.Total != 2 {
		t.Errorf("cash total = %d, want 2", cash.Total)
	}
}
