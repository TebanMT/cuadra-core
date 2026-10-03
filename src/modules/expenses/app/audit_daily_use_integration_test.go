//go:build sidecar

package app_test

import (
	"context"
	"testing"

	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	expense "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	recurring "github.com/cuadra/cuadra-core/src/modules/expenses/domain/recurring"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	repos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	purchase "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	prodRepos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
)

func TestDailyUse_RelatesExistingPaymentAndKeepsActualHistory(t *testing.T) {
	for _, fromCash := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing expense", true: "existing cash out"}[fromCash], func(t *testing.T) {
			f := setupExpenses(t)
			ctx := context.Background()
			day := mexicoBusinessDay(t)
			templates := repos.NewRecurringTemplateSQLiteRepository()
			occurrences := repos.NewOccurrenceSQLiteRepository()
			moves := repos.NewCashMovementSQLiteRepository()
			_, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(ctx, expApp.RecurringTemplateInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: "Limpieza mensual", Category: expense.CategoryRent, ExpectedAmount: 800, PaymentMethod: "cash", Classification: "fixed", Frequency: recurring.Monthly, StartsOn: day})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder).Execute(ctx, f.gymID, day); err != nil {
				t.Fatal(err)
			}
			list := expApp.NewListExpenseOccurrences(occurrences, f.uow).WithDetails(templates, f.expenseRepo)
			rows, err := list.Execute(ctx, expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
			input := expApp.MarkOccurrencePaidInput{GymID: f.gymID, ActorUserID: f.ownerID, OccurrenceID: rows[0].ID, PaidOn: day, Amount: 850, PaymentMethod: "cash", PaidFrom: "cash_drawer", IdempotencyKey: "pay-existing"}
			if fromCash {
				m, e := expApp.NewCreateCashMovement(moves, f.uow, f.recorder, expApp.OperationalPolicy{}).Execute(ctx, expApp.CashMovementInput{GymID: f.gymID, ActorUserID: f.ownerID, MovementOn: day, Amount: 850, MovementType: "cash_out", Reason: "Limpieza mensual"})
				if e != nil {
					t.Fatal(e)
				}
				input.CashMovementID = &m.ID
			} else {
				e, err := expApp.NewCreateExpense(f.expenseRepo, f.uow, f.recorder).WithOperational(moves, expApp.OperationalPolicy{}).Execute(ctx, expApp.CreateExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseDate: day, Amount: 850, Category: expense.CategoryRent, PaymentMethod: "cash", PaidFrom: "cash_drawer", Classification: "fixed", IdempotencyKey: "manual-first"})
				if err != nil {
					t.Fatal(err)
				}
				input.ExistingExpenseID = &e.ExpenseID
			}
			pay := expApp.NewMarkExpenseOccurrencePaid(occurrences, f.expenseRepo, moves, f.uow, f.recorder, expApp.OperationalPolicy{})
			paid, err := pay.Execute(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := pay.Execute(ctx, input)
			if err != nil || replay.ID != paid.ID {
				t.Fatalf("retry=%+v err=%v", replay, err)
			}
			var count int
			var amount int64
			if err = f.db.QueryRowx(`SELECT COUNT(*),SUM(amount) FROM cash_movements WHERE deleted_at IS NULL`).Scan(&count, &amount); err != nil {
				t.Fatal(err)
			}
			if count != 1 || amount != 85000 {
				t.Fatalf("cash count=%d cents=%d", count, amount)
			}
			history, err := list.Execute(ctx, expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if history[0].Name != "Limpieza mensual" || history[0].PaidAmount == nil || *history[0].PaidAmount != 850 || history[0].PaidOn == nil || !history[0].PaidOn.Equal(day) {
				t.Fatalf("history=%+v", history[0])
			}
			update := expApp.NewUpdateExpense(f.expenseRepo, f.uow, f.recorder).WithOperational(moves, expApp.OperationalPolicy{}).WithOccurrences(occurrences)
			err = update.Execute(ctx, expApp.UpdateExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: paid.ID, ExpectedVersion: paid.Version, ExpenseDate: day, Amount: 875, Category: paid.Category, PaymentMethod: "cash", PaidFrom: "cash_drawer", Classification: paid.Classification, CorrectionReason: "El recibo fue por 875"})
			if err != nil {
				t.Fatal(err)
			}
			if err = f.db.QueryRowx(`SELECT COUNT(*),SUM(amount) FROM cash_movements WHERE deleted_at IS NULL`).Scan(&count, &amount); err != nil {
				t.Fatal(err)
			}
			if count != 1 || amount != 87500 {
				t.Fatalf("corrected cash count=%d cents=%d", count, amount)
			}
			history, err = list.Execute(ctx, expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
			if err != nil || *history[0].PaidAmount != 875 {
				t.Fatalf("corrected history=%+v err=%v", history, err)
			}
			// Reclassifying this same physical outflow must reopen its due.
			unclassified, err := expApp.NewUnclassifyCashMovement(moves, f.uow, f.recorder).WithExpenses(f.expenseRepo).WithOccurrences(occurrences).Execute(ctx, expApp.UnclassifyCashMovementInput{GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", MovementID: *paid.CashMovementID, CorrectionReason: "La salida correspondía a otra obligación"})
			if err != nil {
				t.Fatal(err)
			}
			if unclassified.ClassificationStatus != "unclassified" || unclassified.ExpenseID != nil {
				t.Fatalf("movement=%+v", unclassified)
			}
			history, err = list.Execute(ctx, expRepo.OccurrenceQuery{GymID: f.gymID, From: day, To: day, Limit: 10})
			if err != nil || history[0].Status != "pending" || history[0].PaidAmount != nil || history[0].ExpenseID != nil {
				t.Fatalf("reopened history=%+v err=%v", history, err)
			}
			if err = f.db.QueryRowx(`SELECT COUNT(*),SUM(amount) FROM cash_movements WHERE deleted_at IS NULL`).Scan(&count, &amount); err != nil {
				t.Fatal(err)
			}
			if count != 1 || amount != 87500 {
				t.Fatalf("reclassification changed physical cash count=%d cents=%d", count, amount)
			}
		})
	}
}

func TestDailyUse_PaidPurchaseCorrectionIsAtomicAndIdempotent(t *testing.T) {
	f := setupExpenses(t)
	ctx := context.Background()
	day := mexicoBusinessDay(t)
	products := prodRepos.NewProductSQLiteRepository()
	stock := prodRepos.NewStockMovementSQLiteRepository()
	purchases := prodRepos.NewInventoryPurchaseSQLiteRepository()
	cash := expApp.NewInventoryPurchaseCashService(repos.NewCashMovementSQLiteRepository()).WithAudit(f.recorder)
	product, err := prodApp.NewCreateProduct(products, stock, f.uow, f.recorder).Execute(ctx, prodApp.CreateProductInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: "Agua", Price: 20})
	if err != nil {
		t.Fatal(err)
	}
	cost := 5.0
	isPurchase := true
	restock, err := prodApp.NewAdjustStock(products, stock, f.uow, f.recorder).WithPurchases(purchases, cash).Execute(ctx, prodApp.AdjustStockInput{GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", ProductID: product.ProductID, MovementType: "restock", Quantity: 10, Cost: &cost, IsPurchase: &isPurchase, PurchaseStatus: purchase.StatusUnpaid, IdempotencyKey: "initial-stock"})
	if err != nil {
		t.Fatal(err)
	}
	paid, err := prodApp.NewPayInventoryPurchase(purchases, products, cash, f.uow, f.recorder).Execute(ctx, prodApp.PayInventoryPurchaseInput{GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PurchaseID: *restock.PurchaseID, ExpectedVersion: 1, PaidOn: day, PaymentMethod: "cash", PaidFrom: "cash_drawer", IdempotencyKey: "pay-stock"})
	if err != nil {
		t.Fatal(err)
	}
	newCost := 6.0
	input := prodApp.CorrectInventoryPurchaseInput{GymID: f.gymID, ActorUserID: f.ownerID, ActorRole: "owner", PurchaseID: *restock.PurchaseID, ExpectedVersion: paid.Purchase.Version, Quantity: 8, UnitCost: &newCost, CorrectionReason: "Se recibieron ocho a seis pesos", IdempotencyKey: "correct-paid"}
	broken := prodApp.NewCorrectInventoryPurchase(purchases, products, stock, f.uow, failingExpenseAudit{}).WithCash(cash)
	if _, err = broken.Execute(ctx, input); err == nil {
		t.Fatal("expected audit failure")
	}
	assertCash := func(want int64) {
		t.Helper()
		var n int
		var cents int64
		if e := f.db.QueryRowx(`SELECT COUNT(*),COALESCE(SUM(amount),0) FROM cash_movements WHERE deleted_at IS NULL`).Scan(&n, &cents); e != nil {
			t.Fatal(e)
		}
		if n != 1 || cents != want {
			t.Fatalf("cash rows=%d cents=%d want=%d", n, cents, want)
		}
	}
	assertCash(5000)
	good := prodApp.NewCorrectInventoryPurchase(purchases, products, stock, f.uow, f.recorder).WithCash(cash)
	corrected, err := good.Execute(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := good.Execute(ctx, input)
	if err != nil || replay.Version != corrected.Version {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if corrected.Status != "paid" || corrected.PaidOn == nil || *corrected.PaidOn != day.Format("2006-01-02") || corrected.TotalAmount == nil || *corrected.TotalAmount != 48 || corrected.NewStock != 8 {
		t.Fatalf("corrected=%+v", corrected)
	}
	assertCash(4800)
}
