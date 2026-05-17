// Package repository declares persistence contracts for the expenses BC.
// Concrete impls live in infraestructure/db/repositories with build tags.
package repository

import (
	"time"

	"github.com/google/uuid"

	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ExpenseRepository — CRUD + listado con filtros.
type ExpenseRepository interface {
	Create(tx sharedDomain.Transaction, e *expenseDomain.Expense) (*expenseDomain.Expense, error)
	Update(tx sharedDomain.Transaction, e *expenseDomain.Expense) (*expenseDomain.Expense, error)
	GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*expenseDomain.Expense, error)
	List(tx sharedDomain.Transaction, q ListQuery) ([]*expenseDomain.Expense, int, error)
	// ListAggregates devuelve totales sobre el set filtrado completo (no
	// la página visible) — alimenta StatCards en el FE.
	ListAggregates(tx sharedDomain.Transaction, q ListQuery) (ExpenseAggregates, error)
	// ListByDate devuelve todos los gastos cuyo expense_date coincide con
	// el día indicado (UTC, hora ignorada). Usado por el corte de caja del
	// día — no paginado porque un solo día rara vez tiene cientos de filas.
	ListByDate(tx sharedDomain.Transaction, gymID uuid.UUID, day time.Time) ([]*expenseDomain.Expense, error)
}

// ExpenseAggregates — totales globales del filtro. CashTotal y
// NonCashTotal suman amount sobre las filas según payment_method;
// DominantCategory es la categoría con mayor monto acumulado (string
// vacío si no hay filas).
type ExpenseAggregates struct {
	Total            float64
	CashTotal        float64
	NonCashTotal     float64
	DominantCategory string
	DominantCatTotal float64
}

// Sort columns expuestas por el header de la tabla.
const (
	SortDate     = "date"
	SortAmount   = "amount"
	SortCategory = "category"
	SortMethod   = "payment_method"
)

const (
	SortDirAsc  = "asc"
	SortDirDesc = "desc"
)

// ListQuery — filtros aceptados por GET /api/v1/expenses.
type ListQuery struct {
	GymID         uuid.UUID
	From          *time.Time // inclusive
	To            *time.Time // inclusive
	Category      string     // exact match; empty = all
	PaymentMethod string     // exact match; empty = all
	Search        string     // substring sobre description (case-insensitive)
	Sort          string     // SortDate (default) | SortAmount | SortCategory | SortMethod
	Direction     string     // SortDirAsc | SortDirDesc (default desc para SortDate)
	Page          int
	PageSize      int
}
