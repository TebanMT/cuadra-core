package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CreateExpenseInput — captura de un gasto general. Si Description está
// vacío, viaja como nil. ExpenseDate llega ya parseada por el controller.
type CreateExpenseInput struct {
	GymID         uuid.UUID
	ActorUserID   uuid.UUID
	ExpenseDate   time.Time
	Amount        float64
	Category      string
	Description   *string
	PaymentMethod string
}

type CreateExpenseOutput struct {
	ExpenseID uuid.UUID
	Amount    float64
	Category  string
}

type CreateExpense struct {
	Expenses expRepo.ExpenseRepository
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
}

func NewCreateExpense(expenses expRepo.ExpenseRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CreateExpense {
	return &CreateExpense{Expenses: expenses, UoW: uow, Audit: recorder}
}

func (uc *CreateExpense) Execute(ctx context.Context, in CreateExpenseInput) (*CreateExpenseOutput, error) {
	now := time.Now().UTC()
	var out CreateExpenseOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		e, err := expenseDomain.New(uuid.New(), in.GymID, in.ActorUserID, in.ExpenseDate,
			in.Amount, in.Category, in.PaymentMethod, in.Description, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err := uc.Expenses.Create(tx, e); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "expenses",
			EntityID:    e.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"expense_date":   e.ExpenseDate.Format("2006-01-02"),
				"amount":         e.Amount,
				"category":       e.Category,
				"payment_method": e.PaymentMethod,
				"description":    e.Description,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		out = CreateExpenseOutput{ExpenseID: e.ID, Amount: e.Amount, Category: e.Category}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
