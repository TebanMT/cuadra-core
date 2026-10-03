package app

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
)

func TestMoneyCommandsRequireExplicitPaidFromBeforeTouchingDependencies(t *testing.T) {
	ctx := context.Background()
	gymID, actorID := uuid.New(), uuid.New()

	if _, err := (&CreateExpense{}).Execute(ctx, CreateExpenseInput{
		GymID: gymID, ActorUserID: actorID,
	}); !errors.Is(err, expErrors.ErrInvalidPaidFrom) {
		t.Fatalf("create without paid_from error=%v", err)
	}
	if err := (&UpdateExpense{}).Execute(ctx, UpdateExpenseInput{
		GymID: gymID, ActorUserID: actorID, ExpenseID: uuid.New(),
	}); !errors.Is(err, expErrors.ErrInvalidPaidFrom) {
		t.Fatalf("update without paid_from error=%v", err)
	}
	if _, err := (&MarkExpenseOccurrencePaid{}).Execute(ctx, MarkOccurrencePaidInput{
		GymID: gymID, ActorUserID: actorID, OccurrenceID: uuid.New(),
	}); !errors.Is(err, expErrors.ErrInvalidPaidFrom) {
		t.Fatalf("occurrence payment without paid_from error=%v", err)
	}
}
