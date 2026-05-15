package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type UpdateExpenseInput struct {
	GymID         uuid.UUID
	ActorUserID   uuid.UUID
	ExpenseID     uuid.UUID
	ExpenseDate   time.Time
	Amount        float64
	Category      string
	Description   *string
	PaymentMethod string
}

type UpdateExpense struct {
	Expenses expRepo.ExpenseRepository
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
}

func NewUpdateExpense(expenses expRepo.ExpenseRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *UpdateExpense {
	return &UpdateExpense{Expenses: expenses, UoW: uow, Audit: recorder}
}

func (uc *UpdateExpense) Execute(ctx context.Context, in UpdateExpenseInput) error {
	now := time.Now().UTC()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		e, err := uc.Expenses.GetByID(tx, in.ExpenseID)
		if err != nil {
			return err
		}
		if e.GymID != in.GymID {
			return sharedDomain.NewBusinessError(expErrors.ErrCrossGym, "")
		}
		before := map[string]any{
			"expense_date":   e.ExpenseDate.Format("2006-01-02"),
			"amount":         e.Amount,
			"category":       e.Category,
			"payment_method": e.PaymentMethod,
			"description":    e.Description,
		}
		if err := e.Update(in.ExpenseDate, in.Amount, in.Category, in.PaymentMethod, in.Description, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err := uc.Expenses.Update(tx, e); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "expenses",
			EntityID:    e.ID,
			Action:      audit.ActionUpdate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"before": before,
				"after": map[string]any{
					"expense_date":   e.ExpenseDate.Format("2006-01-02"),
					"amount":         e.Amount,
					"category":       e.Category,
					"payment_method": e.PaymentMethod,
					"description":    e.Description,
				},
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		return nil
	})
}
