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

type DeleteExpenseInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	ExpenseID   uuid.UUID
}

type DeleteExpense struct {
	Expenses expRepo.ExpenseRepository
	UoW      sharedDomain.UnitOfWork
	Audit    audit.Recorder
}

func NewDeleteExpense(expenses expRepo.ExpenseRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *DeleteExpense {
	return &DeleteExpense{Expenses: expenses, UoW: uow, Audit: recorder}
}

func (uc *DeleteExpense) Execute(ctx context.Context, in DeleteExpenseInput) error {
	now := time.Now().UTC()
	return uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		e, err := uc.Expenses.GetByID(tx, in.ExpenseID)
		if err != nil {
			return err
		}
		if e.GymID != in.GymID {
			return sharedDomain.NewBusinessError(expErrors.ErrCrossGym, "")
		}
		if e.DeletedAt != nil {
			return nil
		}
		e.SoftDelete(now)
		if _, err := uc.Expenses.Update(tx, e); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "expenses",
			EntityID:    e.ID,
			Action:      audit.ActionDelete,
			ActorUserID: &in.ActorUserID,
			Changes:     map[string]any{"amount": e.Amount, "category": e.Category},
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
}
