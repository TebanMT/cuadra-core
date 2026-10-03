// Package repository declares persistence contracts for the expenses BC.
// Concrete impls live in infraestructure/db/repositories with build tags.
package repository

import (
	"time"

	"github.com/google/uuid"

	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	recurringDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/recurring"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ExpenseRepository — CRUD + listado con filtros.
type ExpenseRepository interface {
	Create(tx sharedDomain.Transaction, e *expenseDomain.Expense) (*expenseDomain.Expense, error)
	Update(tx sharedDomain.Transaction, e *expenseDomain.Expense) (*expenseDomain.Expense, error)
	GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*expenseDomain.Expense, error)
	List(tx sharedDomain.Transaction, q ListQuery) ([]*expenseDomain.Expense, int, error)
	// ListAggregates devuelve totales sobre el set filtrado completo (no
	// la página visible) — alimenta StatCards en el FE.
	ListAggregates(tx sharedDomain.Transaction, q ListQuery) (ExpenseAggregates, error)
	// ListByDate devuelve todos los gastos cuyo expense_date coincide con
	// el día indicado (UTC, hora ignorada). Usado por el corte de caja del
	// día — no paginado porque un solo día rara vez tiene cientos de filas.
	ListByDate(tx sharedDomain.Transaction, gymID uuid.UUID, day time.Time) ([]*expenseDomain.Expense, error)
}

// CashDayLock keeps the expenses context independent from billing internals.
type CashDayLock interface {
	IsClosed(tx sharedDomain.Transaction, gymID uuid.UUID, day time.Time) (bool, error)
}

type CashMovementRepository interface {
	Create(tx sharedDomain.Transaction, m *cashDomain.CashMovement) (*cashDomain.CashMovement, error)
	Update(tx sharedDomain.Transaction, m *cashDomain.CashMovement, expectedVersion int) (*cashDomain.CashMovement, error)
	GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*cashDomain.CashMovement, error)
	GetByExpenseID(tx sharedDomain.Transaction, gymID, expenseID uuid.UUID) (*cashDomain.CashMovement, error)
	List(tx sharedDomain.Transaction, q CashMovementListQuery) ([]*cashDomain.CashMovement, int, error)
	ListByDate(tx sharedDomain.Transaction, gymID uuid.UUID, day time.Time) ([]*cashDomain.CashMovement, error)
	ListUnclassified(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*cashDomain.CashMovement, error)
}

// CashMovementListQuery powers the owner's auditable history. Status is a
// product-facing filter rather than a raw database predicate:
//   - pending/unclassified: cash outs that still need classification
//   - classified: cash outs already owned by an expense, purchase or
//     non-operating withdrawal
//   - expense/inventory_purchase/non_operating: exact classification
//   - all/empty: every physical movement, including cash ins
//
// From and To are inclusive local business dates represented at midnight.
type CashMovementListQuery struct {
	GymID    uuid.UUID
	From     *time.Time
	To       *time.Time
	Status   string
	Page     int
	PageSize int
}

type RecurringTemplateRepository interface {
	Create(tx sharedDomain.Transaction, t *recurringDomain.Template) (*recurringDomain.Template, error)
	Update(tx sharedDomain.Transaction, t *recurringDomain.Template, expectedVersion int) (*recurringDomain.Template, error)
	GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*recurringDomain.Template, error)
	List(tx sharedDomain.Transaction, gymID uuid.UUID, includeInactive bool) ([]*recurringDomain.Template, error)
}

type OccurrenceRepository interface {
	CreateIfAbsent(tx sharedDomain.Transaction, o *recurringDomain.Occurrence) (bool, error)
	Update(tx sharedDomain.Transaction, o *recurringDomain.Occurrence, expectedVersion int) (*recurringDomain.Occurrence, error)
	GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*recurringDomain.Occurrence, error)
	List(tx sharedDomain.Transaction, q OccurrenceQuery) ([]*recurringDomain.Occurrence, error)
	Count(tx sharedDomain.Transaction, q OccurrenceQuery) (int, error)
	HasPendingByTemplate(tx sharedDomain.Transaction, gymID, templateID uuid.UUID) (bool, error)
	UpdatePendingSnapshots(tx sharedDomain.Transaction, t *recurringDomain.Template, from time.Time) error
}

type OccurrenceQuery struct {
	GymID    uuid.UUID
	From, To time.Time
	Status   string
	Limit    int
	Offset   int
}

// ExpenseAggregates — totales globales del filtro. CashTotal y
// NonCashTotal suman amount sobre las filas según payment_method;
// DominantCategory es la categoría con mayor monto acumulado (string
// vacío si no hay filas).
type ExpenseAggregates struct {
	Total            float64
	CashTotal        float64
	NonCashTotal     float64
	FixedTotal       float64
	VariableTotal    float64
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
	GymID          uuid.UUID
	From           *time.Time // inclusive
	To             *time.Time // inclusive
	Category       string     // exact match; empty = all
	PaymentMethod  string     // exact match; empty = all
	Source         string     // manual | recurring | cash_movement; empty = all
	Classification string     // fixed | variable; empty = all
	Search         string     // literal substring over payee/description/reference
	Sort           string     // SortDate (default) | SortAmount | SortCategory | SortMethod
	Direction      string     // SortDirAsc | SortDirDesc (default desc para SortDate)
	Page           int
	PageSize       int
}
