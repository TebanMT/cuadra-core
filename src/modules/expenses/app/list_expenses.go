package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ListExpensesInput — filtros para GET /api/v1/expenses.
type ListExpensesInput struct {
	GymID         uuid.UUID
	From          *time.Time
	To            *time.Time
	Category      string
	PaymentMethod string
	Search        string
	Sort          string
	Direction     string
	Page          int
	PageSize      int
}

type ListExpensesOutput struct {
	Items      []*expenseDomain.Expense
	Total      int
	Page       int
	PageSize   int
	Aggregates expRepo.ExpenseAggregates
}

type ListExpenses struct {
	Expenses expRepo.ExpenseRepository
	UoW      sharedDomain.UnitOfWork
}

func NewListExpenses(expenses expRepo.ExpenseRepository, uow sharedDomain.UnitOfWork) *ListExpenses {
	return &ListExpenses{Expenses: expenses, UoW: uow}
}

func (uc *ListExpenses) Execute(ctx context.Context, in ListExpensesInput) (*ListExpensesOutput, error) {
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	page := in.Page
	if page < 1 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	listQuery := expRepo.ListQuery{
		GymID:         in.GymID,
		From:          in.From,
		To:            in.To,
		Category:      in.Category,
		PaymentMethod: in.PaymentMethod,
		Search:        in.Search,
		Sort:          in.Sort,
		Direction:     in.Direction,
		Page:          page,
		PageSize:      pageSize,
	}
	rows, total, err := uc.Expenses.List(tx, listQuery)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	// Aggregates corre sobre el mismo filtro — alimenta StatCards. Si
	// falla, degrada a ceros (mismo posture que ListProducts).
	aggs, _ := uc.Expenses.ListAggregates(tx, listQuery)
	return &ListExpensesOutput{
		Items:      rows,
		Total:      total,
		Page:       page,
		PageSize:   pageSize,
		Aggregates: aggs,
	}, nil
}
