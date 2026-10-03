//go:build sidecar

package app_test

import (
	"context"
	"testing"
	"time"

	expApp "github.com/cuadra/cuadra-core/src/modules/expenses/app"
	recurring "github.com/cuadra/cuadra-core/src/modules/expenses/domain/recurring"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	repos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
)

func TestRecurring_CatchesUpInChunksAndKeepsOldPendingVisible(t *testing.T) {
	f := setupExpenses(t)
	ctx := context.Background()
	today := mexicoBusinessDay(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	create := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder)
	old := today.AddDate(0, 0, -401)
	weeklyStart := today.AddDate(0, 0, -2814)
	for _, spec := range []struct {
		name, frequency string
		start           time.Time
		end             *time.Time
	}{
		{"Terminada", recurring.Monthly, old, &old}, {"Actual", recurring.Monthly, today, nil}, {"Atrasada", recurring.Weekly, weeklyStart, nil},
	} {
		_, err := create.Execute(ctx, expApp.RecurringTemplateInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: spec.name, Category: "renta", ExpectedAmount: 100, PaymentMethod: "transfer", Classification: "fixed", Frequency: spec.frequency, StartsOn: spec.start, EndsOn: spec.end})
		if err != nil {
			t.Fatal(err)
		}
	}
	materialize := expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder)
	if n, err := materialize.Execute(ctx, f.gymID, today); err != nil || n != 405 {
		t.Fatalf("generated=%d err=%v; want all 405 including the old ended schedule", n, err)
	}
	if n, err := materialize.Execute(ctx, f.gymID, today); err != nil || n != 0 {
		t.Fatalf("replay generated=%d err=%v", n, err)
	}
	list := expApp.NewListExpenseOccurrences(occurrences, f.uow)
	page, err := list.ExecutePage(ctx, expRepo.OccurrenceQuery{GymID: f.gymID, To: today, Status: recurring.Pending}, 1, 200)
	if err != nil || page.Total != 405 || len(page.Items) != 200 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if !page.Items[0].DueOn.Equal(weeklyStart) {
		t.Fatalf("oldest pending lost: %s", page.Items[0].DueOn)
	}
	last, err := list.ExecutePage(ctx, expRepo.OccurrenceQuery{GymID: f.gymID, To: today, Status: recurring.Pending}, 3, 200)
	if err != nil || last.Total != 405 || len(last.Items) != 5 {
		t.Fatalf("last page=%+v err=%v", last, err)
	}
}

func TestRecurring_DeletePaidExpenseAllowsPayingAgain(t *testing.T) {
	f := setupExpenses(t)
	ctx := context.Background()
	day := mexicoBusinessDay(t)
	templates := repos.NewRecurringTemplateSQLiteRepository()
	occurrences := repos.NewOccurrenceSQLiteRepository()
	cash := repos.NewCashMovementSQLiteRepository()
	_, err := expApp.NewCreateRecurringExpenseTemplate(templates, f.uow, f.recorder).Execute(ctx, expApp.RecurringTemplateInput{GymID: f.gymID, ActorUserID: f.ownerID, Name: "Renta", Category: "renta", ExpectedAmount: 100, PaymentMethod: "transfer", Classification: "fixed", Frequency: recurring.Monthly, StartsOn: day})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = expApp.NewMaterializeExpenseOccurrences(templates, occurrences, f.uow, f.recorder).Execute(ctx, f.gymID, day); err != nil {
		t.Fatal(err)
	}
	rows, err := expApp.NewListExpenseOccurrences(occurrences, f.uow).Execute(ctx, expRepo.OccurrenceQuery{GymID: f.gymID, To: day})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	pay := expApp.NewMarkExpenseOccurrencePaid(occurrences, f.expenseRepo, cash, f.uow, f.recorder, expApp.OperationalPolicy{})
	input := expApp.MarkOccurrencePaidInput{GymID: f.gymID, ActorUserID: f.ownerID, OccurrenceID: rows[0].ID, PaidOn: day, Amount: 100, PaymentMethod: "transfer", PaidFrom: "gym_fund", IdempotencyKey: "first"}
	first, err := pay.Execute(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	remove := expApp.NewDeleteExpense(f.expenseRepo, f.uow, f.recorder).WithOccurrences(occurrences).WithOperational(cash, expApp.OperationalPolicy{})
	if err = remove.Execute(ctx, expApp.DeleteExpenseInput{GymID: f.gymID, ActorUserID: f.ownerID, ExpenseID: first.ID, ExpectedVersion: first.Version, CorrectionReason: "Se capturó antes de pagar"}); err != nil {
		t.Fatal(err)
	}
	input.IdempotencyKey = "replacement"
	second, err := pay.Execute(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("the deleted expense was reused")
	}
	var active int
	if err = f.db.Get(&active, `SELECT COUNT(*) FROM expenses WHERE gym_id=? AND deleted_at IS NULL`, f.gymID); err != nil || active != 1 {
		t.Fatalf("active=%d err=%v", active, err)
	}
}
