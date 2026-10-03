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
	ID                    string         `db:"id"`
	GymID                 string         `db:"gym_id"`
	Version               int            `db:"version"`
	CreatedAt             int64          `db:"created_at"`
	UpdatedAt             int64          `db:"updated_at"`
	DeletedAt             sql.NullInt64  `db:"deleted_at"`
	SyncedAt              sql.NullInt64  `db:"synced_at"`
	ExpenseDate           string         `db:"expense_date"`
	Amount                int64          `db:"amount"`
	Category              string         `db:"category"`
	PayeeName             sql.NullString `db:"payee_name"`
	Description           sql.NullString `db:"description"`
	Reference             sql.NullString `db:"reference"`
	PaymentMethod         string         `db:"payment_method"`
	PaidFrom              string         `db:"paid_from"`
	Classification        string         `db:"classification"`
	Source                string         `db:"source"`
	RecurringOccurrenceID sql.NullString `db:"recurring_occurrence_id"`
	CashMovementID        sql.NullString `db:"cash_movement_id"`
	CreatedBy             string         `db:"created_by"`
}

func (r *ExpenseSQLiteRepository) Create(tx sharedDomain.Transaction, e *expenseDomain.Expense) (*expenseDomain.Expense, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := expenseToRow(e)
	const stmt = `
		INSERT INTO expenses (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    expense_date, amount, category, payee_name, description, reference, payment_method,
		    paid_from, classification, source, recurring_occurrence_id, cash_movement_id, created_by
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :expense_date, :amount, :category, :payee_name, :description, :reference, :payment_method,
		    :paid_from, :classification, :source, :recurring_occurrence_id, :cash_movement_id, :created_by
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
		    payee_name=:payee_name, description = :description, reference=:reference,
		    payment_method = :payment_method, paid_from=:paid_from, classification=:classification, source=:source,
		    recurring_occurrence_id=:recurring_occurrence_id, cash_movement_id=:cash_movement_id
		WHERE gym_id=:gym_id AND id = :id AND version=:previous_version`
	params := map[string]any{"id": row.ID, "gym_id": row.GymID, "version": row.Version, "previous_version": row.Version - 1, "updated_at": row.UpdatedAt, "deleted_at": nullInt(row.DeletedAt), "expense_date": row.ExpenseDate, "amount": row.Amount, "category": row.Category, "payee_name": nullString(row.PayeeName), "description": nullString(row.Description), "reference": nullString(row.Reference), "payment_method": row.PaymentMethod, "paid_from": row.PaidFrom, "classification": row.Classification, "source": row.Source, "recurring_occurrence_id": nullString(row.RecurringOccurrenceID), "cash_movement_id": nullString(row.CashMovementID)}
	res, err := stx.NamedExec(context.Background(), stmt, params)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, expErrors.ErrVersionConflict
	}
	if err := enqueueExpense(stx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (r *ExpenseSQLiteRepository) GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*expenseDomain.Expense, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteExpenseRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM expenses WHERE gym_id = ? AND id = ? AND deleted_at IS NULL`, gymID.String(), id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(expErrors.ErrExpenseNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	e := expenseFromRow(&row)
	if err := hydrateExpenseCashDrawersSQLite(tx.(*sharedDomain.SqlxTransaction), gymID, []*expenseDomain.Expense{e}); err != nil {
		return nil, err
	}
	return e, nil
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
	if err := hydrateExpenseCashDrawersSQLite(stx, q.GymID, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (r *ExpenseSQLiteRepository) ListAggregates(tx sharedDomain.Transaction, q expRepo.ListQuery) (expRepo.ExpenseAggregates, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	whereClause, args := buildExpenseWhereSqlite(q)
	var row struct {
		TotalCents    sql.NullInt64 `db:"total"`
		CashCents     sql.NullInt64 `db:"cash_total"`
		NonCashCents  sql.NullInt64 `db:"non_cash_total"`
		FixedCents    sql.NullInt64 `db:"fixed_total"`
		VariableCents sql.NullInt64 `db:"variable_total"`
	}
	stmt := fmt.Sprintf(`
		SELECT
		  COALESCE(SUM(amount), 0) AS total,
		  COALESCE(SUM(CASE WHEN payment_method = 'cash' THEN amount ELSE 0 END), 0) AS cash_total,
		  COALESCE(SUM(CASE WHEN payment_method <> 'cash' THEN amount ELSE 0 END), 0) AS non_cash_total,
		  COALESCE(SUM(CASE WHEN classification = 'fixed' THEN amount ELSE 0 END), 0) AS fixed_total,
		  COALESCE(SUM(CASE WHEN classification = 'variable' THEN amount ELSE 0 END), 0) AS variable_total
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
		Total:         fromCents(row.TotalCents.Int64),
		CashTotal:     fromCents(row.CashCents.Int64),
		NonCashTotal:  fromCents(row.NonCashCents.Int64),
		FixedTotal:    fromCents(row.FixedCents.Int64),
		VariableTotal: fromCents(row.VariableCents.Int64),
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
	if err := hydrateExpenseCashDrawersSQLite(stx, gymID, out); err != nil {
		return nil, err
	}
	return out, nil
}

func hydrateExpenseCashDrawersSQLite(tx *sharedDomain.SqlxTransaction, gymID uuid.UUID, expenses []*expenseDomain.Expense) error {
	ids := make([]uuid.UUID, 0, len(expenses))
	byMovement := make(map[uuid.UUID]*expenseDomain.Expense, len(expenses))
	for _, e := range expenses {
		if e != nil && e.CashMovementID != nil {
			ids = append(ids, *e.CashMovementID)
			byMovement[*e.CashMovementID] = e
		}
	}
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, gymID.String())
	marks := make([]string, len(ids))
	for i, id := range ids {
		marks[i] = "?"
		args = append(args, id.String())
	}
	var rows []struct {
		ID           string `db:"id"`
		CashDrawerID string `db:"cash_drawer_id"`
	}
	query := `SELECT id,COALESCE(cash_drawer_id,gym_id) AS cash_drawer_id FROM cash_movements
		WHERE gym_id=? AND deleted_at IS NULL AND id IN (` + strings.Join(marks, ",") + `)`
	if err := tx.Select(context.Background(), &rows, query, args...); err != nil {
		return err
	}
	for _, row := range rows {
		movementID, movementErr := uuid.Parse(row.ID)
		drawerID, drawerErr := uuid.Parse(row.CashDrawerID)
		if movementErr != nil || drawerErr != nil {
			continue
		}
		if e := byMovement[movementID]; e != nil {
			d := drawerID
			e.CashDrawerID = &d
		}
	}
	return nil
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
	if q.Source != "" {
		where = append(where, "source = ?")
		args = append(args, q.Source)
	}
	if q.Classification != "" {
		where = append(where, "classification = ?")
		args = append(args, q.Classification)
	}
	if s := normalizeExpenseSearch(q.Search); s != "" {
		pattern := "%" + escapeLike(s) + "%"
		where = append(where, "("+sqliteNormalizedSearchColumn("payee_name")+" LIKE ? ESCAPE '!' OR "+sqliteNormalizedSearchColumn("description")+" LIKE ? ESCAPE '!' OR "+sqliteNormalizedSearchColumn("reference")+" LIKE ? ESCAPE '!')")
		args = append(args, pattern, pattern, pattern)
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
		return col + " " + direction + ", created_at DESC, id ASC"
	}
	return col + " " + direction + ", expense_date DESC, created_at DESC, id ASC"
}

func expenseToRow(e *expenseDomain.Expense) sqliteExpenseRow {
	row := sqliteExpenseRow{
		ID:             e.ID.String(),
		GymID:          e.GymID.String(),
		Version:        e.Version,
		CreatedAt:      e.CreatedAt.UnixMilli(),
		UpdatedAt:      e.UpdatedAt.UnixMilli(),
		ExpenseDate:    e.ExpenseDate.Format("2006-01-02"),
		Amount:         toCents(e.Amount),
		Category:       e.Category,
		Classification: e.Classification,
		Source:         e.Source,
		PaymentMethod:  e.PaymentMethod,
		PaidFrom:       e.PaidFrom,
		CreatedBy:      e.CreatedBy.String(),
	}
	if e.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: e.DeletedAt.UnixMilli(), Valid: true}
	}
	if e.Description != nil {
		row.Description = sql.NullString{String: *e.Description, Valid: true}
	}
	if e.PayeeName != nil {
		row.PayeeName = sql.NullString{String: *e.PayeeName, Valid: true}
	}
	if e.Reference != nil {
		row.Reference = sql.NullString{String: *e.Reference, Valid: true}
	}
	if e.RecurringOccurrenceID != nil {
		row.RecurringOccurrenceID = sql.NullString{String: e.RecurringOccurrenceID.String(), Valid: true}
	}
	if e.CashMovementID != nil {
		row.CashMovementID = sql.NullString{String: e.CashMovementID.String(), Valid: true}
	}
	return row
}

func expenseFromRow(r *sqliteExpenseRow) *expenseDomain.Expense {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	createdBy, _ := uuid.Parse(r.CreatedBy)
	date, _ := time.Parse("2006-01-02", r.ExpenseDate)
	e := &expenseDomain.Expense{
		ID:             id,
		GymID:          gymID,
		Version:        r.Version,
		ExpenseDate:    date,
		PaidOn:         date,
		Amount:         fromCents(r.Amount),
		Category:       r.Category,
		PaymentMethod:  r.PaymentMethod,
		PaidFrom:       r.PaidFrom,
		Classification: r.Classification, Source: r.Source,
		CreatedBy: createdBy,
		CreatedAt: time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt: time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		e.DeletedAt = &t
	}
	if r.Description.Valid {
		d := r.Description.String
		e.Description = &d
	}
	if r.PayeeName.Valid {
		e.PayeeName = &r.PayeeName.String
	}
	if r.Reference.Valid {
		e.Reference = &r.Reference.String
	}
	e.RecurringOccurrenceID = parseUUIDPtr(r.RecurringOccurrenceID)
	e.CashMovementID = parseUUIDPtr(r.CashMovementID)
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
		"deleted_at":     nullableMillis(e.DeletedAt),
		"expense_date":   e.ExpenseDate.Format("2006-01-02"),
		"amount":         e.Amount,
		"category":       e.Category,
		"payee_name":     e.PayeeName,
		"description":    strPtrOrNil(e.Description),
		"reference":      e.Reference,
		"payment_method": e.PaymentMethod,
		"paid_from":      e.PaidFrom,
		"classification": e.Classification, "source": e.Source, "recurring_occurrence_id": e.RecurringOccurrenceID, "cash_movement_id": e.CashMovementID,
		"created_by": e.CreatedBy.String(),
	})
	if err != nil {
		return err
	}
	operation := "upsert"
	if e.DeletedAt != nil {
		operation = "delete"
	}
	return stx.EnqueueSync(context.Background(), "expenses", e.ID.String(), operation, payload, e.Version)
}

func nullString(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}
func nullInt(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nullableMillis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func strPtrOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
