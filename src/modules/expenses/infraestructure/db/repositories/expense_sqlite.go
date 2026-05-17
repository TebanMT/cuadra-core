//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// SQLite guarda money en cents (ADR-002 §2). Convierte al edge.
func toCents(v float64) int64   { return int64(math.Round(v * 100)) }
func fromCents(c int64) float64 { return float64(c) / 100 }

type ExpenseSQLiteRepository struct{}

func NewExpenseSQLiteRepository() *ExpenseSQLiteRepository { return &ExpenseSQLiteRepository{} }

type sqliteExpenseRow struct {
	ID            string         `db:"id"`
	GymID         string         `db:"gym_id"`
	Version       int            `db:"version"`
	CreatedAt     int64          `db:"created_at"`
	UpdatedAt     int64          `db:"updated_at"`
	DeletedAt     sql.NullInt64  `db:"deleted_at"`
	SyncedAt      sql.NullInt64  `db:"synced_at"`
	ExpenseDate   string         `db:"expense_date"`
	Amount        int64          `db:"amount"`
	Category      string         `db:"category"`
	Description   sql.NullString `db:"description"`
	PaymentMethod string         `db:"payment_method"`
	CreatedBy     string         `db:"created_by"`
}

func (r *ExpenseSQLiteRepository) Create(tx sharedDomain.Transaction, e *expenseDomain.Expense) (*expenseDomain.Expense, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := expenseToRow(e)
	const stmt = `
		INSERT INTO expenses (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    expense_date, amount, category, description, payment_method, created_by
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :expense_date, :amount, :category, :description, :payment_method, :created_by
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueExpense(stx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (r *ExpenseSQLiteRepository) Update(tx sharedDomain.Transaction, e *expenseDomain.Expense) (*expenseDomain.Expense, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	e.UpdatedAt = time.Now().UTC()
	row := expenseToRow(e)
	const stmt = `
		UPDATE expenses SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    expense_date = :expense_date, amount = :amount, category = :category,
		    description = :description, payment_method = :payment_method
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueExpense(stx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (r *ExpenseSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*expenseDomain.Expense, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteExpenseRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM expenses WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(expErrors.ErrExpenseNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return expenseFromRow(&row), nil
}

func (r *ExpenseSQLiteRepository) List(tx sharedDomain.Transaction, q expRepo.ListQuery) ([]*expenseDomain.Expense, int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	page, pageSize := normalizePage(q.Page, q.PageSize)
	whereClause, args := buildExpenseWhereSqlite(q)
	var total int
	if err := stx.Get(context.Background(), &total,
		`SELECT COUNT(*) FROM expenses WHERE `+whereClause, args...); err != nil {
		return nil, 0, err
	}
	q2 := fmt.Sprintf(
		`SELECT * FROM expenses WHERE %s ORDER BY %s LIMIT %d OFFSET %d`,
		whereClause, sortClauseSqlite(q.Sort, q.Direction), pageSize, (page-1)*pageSize)
	var rows []sqliteExpenseRow
	if err := stx.Select(context.Background(), &rows, q2, args...); err != nil {
		return nil, 0, err
	}
	out := make([]*expenseDomain.Expense, len(rows))
	for i := range rows {
		out[i] = expenseFromRow(&rows[i])
	}
	return out, total, nil
}

func (r *ExpenseSQLiteRepository) ListAggregates(tx sharedDomain.Transaction, q expRepo.ListQuery) (expRepo.ExpenseAggregates, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	whereClause, args := buildExpenseWhereSqlite(q)
	var row struct {
		TotalCents   sql.NullInt64 `db:"total"`
		CashCents    sql.NullInt64 `db:"cash_total"`
		NonCashCents sql.NullInt64 `db:"non_cash_total"`
	}
	stmt := fmt.Sprintf(`
		SELECT
		  COALESCE(SUM(amount), 0) AS total,
		  COALESCE(SUM(CASE WHEN payment_method = 'cash' THEN amount ELSE 0 END), 0) AS cash_total,
		  COALESCE(SUM(CASE WHEN payment_method <> 'cash' THEN amount ELSE 0 END), 0) AS non_cash_total
		FROM expenses WHERE %s`, whereClause)
	if err := stx.Get(context.Background(), &row, stmt, args...); err != nil {
		return expRepo.ExpenseAggregates{}, err
	}
	type catRow struct {
		Category string `db:"category"`
		Total    int64  `db:"total"`
	}
	var cats []catRow
	catStmt := fmt.Sprintf(`
		SELECT category, COALESCE(SUM(amount), 0) AS total
		FROM expenses WHERE %s
		GROUP BY category
		ORDER BY total DESC
		LIMIT 1`, whereClause)
	if err := stx.Select(context.Background(), &cats, catStmt, args...); err != nil {
		return expRepo.ExpenseAggregates{}, err
	}
	out := expRepo.ExpenseAggregates{
		Total:        fromCents(row.TotalCents.Int64),
		CashTotal:    fromCents(row.CashCents.Int64),
		NonCashTotal: fromCents(row.NonCashCents.Int64),
	}
	if len(cats) > 0 {
		out.DominantCategory = cats[0].Category
		out.DominantCatTotal = fromCents(cats[0].Total)
	}
	return out, nil
}

// ListByDate — gastos cuyo expense_date coincide con el día indicado,
// ordenados por created_at DESC. Sin paginación.
func (r *ExpenseSQLiteRepository) ListByDate(tx sharedDomain.Transaction, gymID uuid.UUID, day time.Time) ([]*expenseDomain.Expense, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	dateStr := day.UTC().Format("2006-01-02")
	var rows []sqliteExpenseRow
	if err := stx.Select(context.Background(), &rows,
		`SELECT * FROM expenses
		 WHERE gym_id = ? AND deleted_at IS NULL AND expense_date = ?
		 ORDER BY created_at DESC`,
		gymID.String(), dateStr); err != nil {
		return nil, err
	}
	out := make([]*expenseDomain.Expense, len(rows))
	for i := range rows {
		out[i] = expenseFromRow(&rows[i])
	}
	return out, nil
}

func buildExpenseWhereSqlite(q expRepo.ListQuery) (string, []any) {
	where := []string{"gym_id = ?", "deleted_at IS NULL"}
	args := []any{q.GymID.String()}
	if q.From != nil {
		where = append(where, "expense_date >= ?")
		args = append(args, q.From.Format("2006-01-02"))
	}
	if q.To != nil {
		where = append(where, "expense_date <= ?")
		args = append(args, q.To.Format("2006-01-02"))
	}
	if q.Category != "" {
		where = append(where, "category = ?")
		args = append(args, q.Category)
	}
	if q.PaymentMethod != "" {
		where = append(where, "payment_method = ?")
		args = append(args, q.PaymentMethod)
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		where = append(where, "description LIKE ? COLLATE NOCASE")
		args = append(args, "%"+s+"%")
	}
	return strings.Join(where, " AND "), args
}

func sortClauseSqlite(sort, dir string) string {
	col := "expense_date"
	switch sort {
	case expRepo.SortAmount:
		col = "amount"
	case expRepo.SortCategory:
		col = "category COLLATE NOCASE"
	case expRepo.SortMethod:
		col = "payment_method"
	}
	// Default desc para fecha; el resto default asc. Coincide con la
	// versión Postgres.
	direction := "ASC"
	if dir == expRepo.SortDirDesc || (dir == "" && (sort == "" || sort == expRepo.SortDate)) {
		direction = "DESC"
	}
	if dir == expRepo.SortDirAsc {
		direction = "ASC"
	}
	if sort == expRepo.SortDate || sort == "" {
		return col + " " + direction + ", created_at DESC"
	}
	return col + " " + direction + ", expense_date DESC, created_at DESC"
}

func expenseToRow(e *expenseDomain.Expense) sqliteExpenseRow {
	row := sqliteExpenseRow{
		ID:            e.ID.String(),
		GymID:         e.GymID.String(),
		Version:       e.Version,
		CreatedAt:     e.CreatedAt.UnixMilli(),
		UpdatedAt:     e.UpdatedAt.UnixMilli(),
		ExpenseDate:   e.ExpenseDate.Format("2006-01-02"),
		Amount:        toCents(e.Amount),
		Category:      e.Category,
		PaymentMethod: e.PaymentMethod,
		CreatedBy:     e.CreatedBy.String(),
	}
	if e.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: e.DeletedAt.UnixMilli(), Valid: true}
	}
	if e.Description != nil {
		row.Description = sql.NullString{String: *e.Description, Valid: true}
	}
	return row
}

func expenseFromRow(r *sqliteExpenseRow) *expenseDomain.Expense {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	createdBy, _ := uuid.Parse(r.CreatedBy)
	date, _ := time.Parse("2006-01-02", r.ExpenseDate)
	e := &expenseDomain.Expense{
		ID:            id,
		GymID:         gymID,
		Version:       r.Version,
		ExpenseDate:   date,
		Amount:        fromCents(r.Amount),
		Category:      r.Category,
		PaymentMethod: r.PaymentMethod,
		CreatedBy:     createdBy,
		CreatedAt:     time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:     time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		e.DeletedAt = &t
	}
	if r.Description.Valid {
		d := r.Description.String
		e.Description = &d
	}
	return e
}

func enqueueExpense(stx *sharedDomain.SqlxTransaction, e *expenseDomain.Expense) error {
	if stx.Queue == nil {
		return nil
	}
	// Todas las columnas NOT NULL deben viajar en el payload — el
	// projector cloud hace UPSERT solo con las keys presentes; omitir
	// una required dispara 23502 en el INSERT inicial.
	payload, err := json.Marshal(map[string]any{
		"id":             e.ID.String(),
		"gym_id":         e.GymID.String(),
		"version":        e.Version,
		"created_at":     e.CreatedAt.UnixMilli(),
		"updated_at":     e.UpdatedAt.UnixMilli(),
		"expense_date":   e.ExpenseDate.Format("2006-01-02"),
		"amount":         e.Amount,
		"category":       e.Category,
		"description":    strPtrOrNil(e.Description),
		"payment_method": e.PaymentMethod,
		"created_by":     e.CreatedBy.String(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "expenses", e.ID.String(), "upsert", payload, e.Version)
}

func strPtrOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
