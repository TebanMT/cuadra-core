//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	recurring "github.com/cuadra/cuadra-core/src/modules/expenses/domain/recurring"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"strings"
	"time"
)

type CashDayLockSQLite struct{}

func NewCashDayLockSQLite() *CashDayLockSQLite { return &CashDayLockSQLite{} }
func (*CashDayLockSQLite) IsClosed(tx shared.Transaction, gymID uuid.UUID, day time.Time) (bool, error) {
	var n int
	err := tx.(*shared.SqlxTransaction).Get(context.Background(), &n, `SELECT COUNT(*) FROM cash_close_events WHERE gym_id=? AND close_date=? AND deleted_at IS NULL`, gymID.String(), day.Format("2006-01-02"))
	return n > 0, err
}

type CashMovementSQLiteRepository struct{}

func NewCashMovementSQLiteRepository() *CashMovementSQLiteRepository {
	return &CashMovementSQLiteRepository{}
}

type cashRow struct {
	ID           string         `db:"id"`
	GymID        string         `db:"gym_id"`
	CashDrawerID sql.NullString `db:"cash_drawer_id"`
	Version      int            `db:"version"`
	CreatedAt    int64          `db:"created_at"`
	UpdatedAt    int64          `db:"updated_at"`
	DeletedAt    sql.NullInt64  `db:"deleted_at"`
	MovementOn   string         `db:"movement_on"`
	Amount       int64          `db:"amount"`
	MovementType string         `db:"movement_type"`
	Reason       string         `db:"reason"`
	OperatorID   string         `db:"operator_id"`
	ExpenseID    sql.NullString `db:"expense_id"`
	Status       string         `db:"classification_status"`
}

func (r *CashMovementSQLiteRepository) Create(tx shared.Transaction, m *cashDomain.CashMovement) (*cashDomain.CashMovement, error) {
	s := tx.(*shared.SqlxTransaction)
	_, err := s.Exec(context.Background(), `INSERT INTO cash_movements(id,gym_id,version,created_at,updated_at,deleted_at,movement_on,amount,movement_type,reason,operator_id,expense_id,classification_status,cash_drawer_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, m.ID, m.GymID, m.Version, m.CreatedAt.UnixMilli(), m.UpdatedAt.UnixMilli(), nil, m.MovementOn.Format("2006-01-02"), toCents(m.Amount), m.MovementType, m.Reason, m.OperatorID, uuidPtrString(m.ExpenseID), m.ClassificationStatus, cashMovementDrawerID(m))
	if err != nil {
		return nil, err
	}
	if err = enqueueFinancial(s, "cash_movements", m.ID, m.GymID, m.Version, m.DeletedAt, cashPayload(m)); err != nil {
		return nil, err
	}
	return m, nil
}
func (r *CashMovementSQLiteRepository) Update(tx shared.Transaction, m *cashDomain.CashMovement, expected int) (*cashDomain.CashMovement, error) {
	s := tx.(*shared.SqlxTransaction)
	res, err := s.Exec(context.Background(), `UPDATE cash_movements SET version=?,updated_at=?,deleted_at=?,movement_on=?,amount=?,movement_type=?,reason=?,expense_id=?,classification_status=?,cash_drawer_id=? WHERE gym_id=? AND id=? AND version=?`, m.Version, m.UpdatedAt.UnixMilli(), millisPtr(m.DeletedAt), m.MovementOn.Format("2006-01-02"), toCents(m.Amount), m.MovementType, m.Reason, uuidPtrString(m.ExpenseID), m.ClassificationStatus, cashMovementDrawerID(m), m.GymID, m.ID, expected)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, expErrors.ErrVersionConflict
	}
	if err = enqueueFinancial(s, "cash_movements", m.ID, m.GymID, m.Version, m.DeletedAt, cashPayload(m)); err != nil {
		return nil, err
	}
	return m, nil
}
func (r *CashMovementSQLiteRepository) GetByID(tx shared.Transaction, gymID, id uuid.UUID) (*cashDomain.CashMovement, error) {
	var row cashRow
	err := tx.(*shared.SqlxTransaction).Get(context.Background(), &row, `SELECT id,gym_id,version,created_at,updated_at,deleted_at,movement_on,amount,movement_type,reason,operator_id,expense_id,classification_status,cash_drawer_id FROM cash_movements WHERE gym_id=? AND id=? AND deleted_at IS NULL`, gymID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, expErrors.ErrCashMovementNotFound
	}
	if err != nil {
		return nil, err
	}
	return cashFromRow(row), nil
}
func (r *CashMovementSQLiteRepository) GetByExpenseID(tx shared.Transaction, gymID, expenseID uuid.UUID) (*cashDomain.CashMovement, error) {
	var row cashRow
	err := tx.(*shared.SqlxTransaction).Get(context.Background(), &row, `SELECT id,gym_id,version,created_at,updated_at,deleted_at,movement_on,amount,movement_type,reason,operator_id,expense_id,classification_status,cash_drawer_id FROM cash_movements WHERE gym_id=? AND expense_id=? AND deleted_at IS NULL`, gymID, expenseID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, expErrors.ErrCashMovementNotFound
	}
	if err != nil {
		return nil, err
	}
	return cashFromRow(row), nil
}
func (r *CashMovementSQLiteRepository) List(tx shared.Transaction, q expRepo.CashMovementListQuery) ([]*cashDomain.CashMovement, int, error) {
	page, pageSize := normalizePage(q.Page, q.PageSize)
	where := []string{"gym_id=?", "deleted_at IS NULL"}
	args := []any{q.GymID.String()}
	if q.From != nil {
		where = append(where, "movement_on>=?")
		args = append(args, q.From.Format("2006-01-02"))
	}
	if q.To != nil {
		where = append(where, "movement_on<=?")
		args = append(args, q.To.Format("2006-01-02"))
	}
	switch q.Status {
	case "pending", cashDomain.Unclassified:
		where = append(where, "movement_type='cash_out'", "classification_status='unclassified'")
	case "classified":
		where = append(where, "classification_status<>'unclassified'")
	case cashDomain.AsExpense, cashDomain.AsInventoryPurchase, cashDomain.NonOperating:
		where = append(where, "classification_status=?")
		args = append(args, q.Status)
	}
	clause := strings.Join(where, " AND ")
	var total int
	if err := tx.(*shared.SqlxTransaction).Get(context.Background(), &total,
		`SELECT COUNT(*) FROM cash_movements WHERE `+clause, args...); err != nil {
		return nil, 0, err
	}
	listArgs := append(append([]any(nil), args...), pageSize, (page-1)*pageSize)
	var rows []cashRow
	err := tx.(*shared.SqlxTransaction).Select(context.Background(), &rows,
		`SELECT id,gym_id,version,created_at,updated_at,deleted_at,movement_on,amount,movement_type,reason,operator_id,expense_id,classification_status,cash_drawer_id
		 FROM cash_movements WHERE `+clause+`
		 ORDER BY movement_on DESC,created_at DESC,id DESC LIMIT ? OFFSET ?`, listArgs...)
	return cashRows(rows), total, err
}
func (r *CashMovementSQLiteRepository) ListByDate(tx shared.Transaction, gymID uuid.UUID, day time.Time) ([]*cashDomain.CashMovement, error) {
	var rows []cashRow
	err := tx.(*shared.SqlxTransaction).Select(context.Background(), &rows, `SELECT id,gym_id,version,created_at,updated_at,deleted_at,movement_on,amount,movement_type,reason,operator_id,expense_id,classification_status,cash_drawer_id FROM cash_movements WHERE gym_id=? AND movement_on=? AND deleted_at IS NULL ORDER BY created_at,id`, gymID, day.Format("2006-01-02"))
	return cashRows(rows), err
}
func (r *CashMovementSQLiteRepository) ListUnclassified(tx shared.Transaction, gymID uuid.UUID, limit int) ([]*cashDomain.CashMovement, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	var rows []cashRow
	err := tx.(*shared.SqlxTransaction).Select(context.Background(), &rows, `SELECT id,gym_id,version,created_at,updated_at,deleted_at,movement_on,amount,movement_type,reason,operator_id,expense_id,classification_status,cash_drawer_id FROM cash_movements WHERE gym_id=? AND movement_type='cash_out' AND classification_status='unclassified' AND deleted_at IS NULL ORDER BY movement_on,created_at,id LIMIT ?`, gymID, limit)
	return cashRows(rows), err
}
func cashRows(rows []cashRow) []*cashDomain.CashMovement {
	out := make([]*cashDomain.CashMovement, len(rows))
	for i, x := range rows {
		out[i] = cashFromRow(x)
	}
	return out
}
func cashFromRow(x cashRow) *cashDomain.CashMovement {
	id, _ := uuid.Parse(x.ID)
	g, _ := uuid.Parse(x.GymID)
	op, _ := uuid.Parse(x.OperatorID)
	d, _ := time.Parse("2006-01-02", x.MovementOn)
	drawerID := g
	if x.CashDrawerID.Valid {
		drawerID, _ = uuid.Parse(x.CashDrawerID.String)
	}
	m := &cashDomain.CashMovement{ID: id, GymID: g, CashDrawerID: drawerID, Version: x.Version, MovementOn: d, Amount: fromCents(x.Amount), MovementType: x.MovementType, Reason: x.Reason, OperatorID: op, ClassificationStatus: x.Status, CreatedAt: time.UnixMilli(x.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(x.UpdatedAt).UTC()}
	if x.ExpenseID.Valid {
		v, _ := uuid.Parse(x.ExpenseID.String)
		m.ExpenseID = &v
	}
	if x.DeletedAt.Valid {
		v := time.UnixMilli(x.DeletedAt.Int64).UTC()
		m.DeletedAt = &v
	}
	return m
}
func cashPayload(m *cashDomain.CashMovement) map[string]any {
	return map[string]any{"id": m.ID, "gym_id": m.GymID, "version": m.Version, "created_at": m.CreatedAt.UnixMilli(), "updated_at": m.UpdatedAt.UnixMilli(), "deleted_at": millisPtr(m.DeletedAt), "movement_on": m.MovementOn.Format("2006-01-02"), "amount": m.Amount, "movement_type": m.MovementType, "reason": m.Reason, "operator_id": m.OperatorID, "expense_id": m.ExpenseID, "classification_status": m.ClassificationStatus, "cash_drawer_id": cashMovementDrawerID(m)}
}

func cashMovementDrawerID(m *cashDomain.CashMovement) string {
	if m.CashDrawerID != uuid.Nil {
		return m.CashDrawerID.String()
	}
	return m.GymID.String()
}

type RecurringTemplateSQLiteRepository struct{}

func NewRecurringTemplateSQLiteRepository() *RecurringTemplateSQLiteRepository {
	return &RecurringTemplateSQLiteRepository{}
}

type templateRow struct {
	ID             string         `db:"id"`
	GymID          string         `db:"gym_id"`
	Version        int            `db:"version"`
	CreatedAt      int64          `db:"created_at"`
	UpdatedAt      int64          `db:"updated_at"`
	DeletedAt      sql.NullInt64  `db:"deleted_at"`
	Name           string         `db:"name"`
	Payee          sql.NullString `db:"payee_name"`
	Category       string         `db:"category"`
	Amount         int64          `db:"expected_amount"`
	Method         string         `db:"usual_payment_method"`
	Classification string         `db:"classification"`
	Frequency      string         `db:"frequency"`
	StartsOn       string         `db:"starts_on"`
	NextDueOn      string         `db:"next_due_on"`
	EndsOn         sql.NullString `db:"ends_on"`
	Active         bool           `db:"active"`
	CreatedBy      string         `db:"created_by"`
}

const templateCols = `id,gym_id,version,created_at,updated_at,deleted_at,name,payee_name,category,expected_amount,usual_payment_method,classification,frequency,starts_on,ends_on,next_due_on,active,created_by`

func (r *RecurringTemplateSQLiteRepository) Create(tx shared.Transaction, t *recurring.Template) (*recurring.Template, error) {
	s := tx.(*shared.SqlxTransaction)
	_, err := s.Exec(context.Background(), `INSERT INTO recurring_expense_templates(`+templateCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, t.ID, t.GymID, t.Version, t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli(), nil, t.Name, strPtrOrNil(t.PayeeName), t.Category, toCents(t.ExpectedAmount), t.PaymentMethod, t.Classification, t.Frequency, t.StartsOn.Format("2006-01-02"), datePtr(t.EndsOn), t.NextDueOn.Format("2006-01-02"), t.Active, t.CreatedBy)
	if err != nil {
		return nil, err
	}
	if err = enqueueFinancial(s, "recurring_expense_templates", t.ID, t.GymID, t.Version, t.DeletedAt, templatePayload(t)); err != nil {
		return nil, err
	}
	return t, nil
}
func (r *RecurringTemplateSQLiteRepository) Update(tx shared.Transaction, t *recurring.Template, expected int) (*recurring.Template, error) {
	s := tx.(*shared.SqlxTransaction)
	res, err := s.Exec(context.Background(), `UPDATE recurring_expense_templates SET version=?,updated_at=?,deleted_at=?,name=?,payee_name=?,category=?,expected_amount=?,usual_payment_method=?,classification=?,frequency=?,starts_on=?,ends_on=?,next_due_on=?,active=? WHERE gym_id=? AND id=? AND version=?`, t.Version, t.UpdatedAt.UnixMilli(), millisPtr(t.DeletedAt), t.Name, strPtrOrNil(t.PayeeName), t.Category, toCents(t.ExpectedAmount), t.PaymentMethod, t.Classification, t.Frequency, t.StartsOn.Format("2006-01-02"), datePtr(t.EndsOn), t.NextDueOn.Format("2006-01-02"), t.Active, t.GymID, t.ID, expected)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, expErrors.ErrVersionConflict
	}
	if err = enqueueFinancial(s, "recurring_expense_templates", t.ID, t.GymID, t.Version, t.DeletedAt, templatePayload(t)); err != nil {
		return nil, err
	}
	return t, nil
}
func (r *RecurringTemplateSQLiteRepository) GetByID(tx shared.Transaction, gymID, id uuid.UUID) (*recurring.Template, error) {
	var row templateRow
	err := tx.(*shared.SqlxTransaction).Get(context.Background(), &row, `SELECT `+templateCols+` FROM recurring_expense_templates WHERE gym_id=? AND id=? AND deleted_at IS NULL`, gymID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, expErrors.ErrTemplateNotFound
	}
	if err != nil {
		return nil, err
	}
	return templateFromRow(row), nil
}
func (r *RecurringTemplateSQLiteRepository) List(tx shared.Transaction, gymID uuid.UUID, include bool) ([]*recurring.Template, error) {
	where := "gym_id=? AND deleted_at IS NULL"
	if !include {
		where += " AND active=1"
	}
	var rows []templateRow
	err := tx.(*shared.SqlxTransaction).Select(context.Background(), &rows, `SELECT `+templateCols+` FROM recurring_expense_templates WHERE `+where+` ORDER BY active DESC,next_due_on,name,id`, gymID)
	out := make([]*recurring.Template, len(rows))
	for i, x := range rows {
		out[i] = templateFromRow(x)
	}
	return out, err
}
func templateFromRow(x templateRow) *recurring.Template {
	id, _ := uuid.Parse(x.ID)
	g, _ := uuid.Parse(x.GymID)
	cb, _ := uuid.Parse(x.CreatedBy)
	st, _ := time.Parse("2006-01-02", x.StartsOn)
	nd, _ := time.Parse("2006-01-02", x.NextDueOn)
	t := &recurring.Template{ID: id, GymID: g, Version: x.Version, Name: x.Name, Category: x.Category, ExpectedAmount: fromCents(x.Amount), PaymentMethod: x.Method, Classification: x.Classification, Frequency: x.Frequency, StartsOn: st, NextDueOn: nd, Active: x.Active, CreatedBy: cb, CreatedAt: time.UnixMilli(x.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(x.UpdatedAt).UTC()}
	if x.Payee.Valid {
		t.PayeeName = &x.Payee.String
	}
	if x.EndsOn.Valid {
		v, _ := time.Parse("2006-01-02", x.EndsOn.String)
		t.EndsOn = &v
	}
	if x.DeletedAt.Valid {
		v := time.UnixMilli(x.DeletedAt.Int64).UTC()
		t.DeletedAt = &v
	}
	return t
}
func templatePayload(t *recurring.Template) map[string]any {
	return map[string]any{"id": t.ID, "gym_id": t.GymID, "version": t.Version, "created_at": t.CreatedAt.UnixMilli(), "updated_at": t.UpdatedAt.UnixMilli(), "deleted_at": millisPtr(t.DeletedAt), "name": t.Name, "payee_name": t.PayeeName, "category": t.Category, "expected_amount": t.ExpectedAmount, "usual_payment_method": t.PaymentMethod, "classification": t.Classification, "frequency": t.Frequency, "starts_on": t.StartsOn.Format("2006-01-02"), "ends_on": datePtr(t.EndsOn), "next_due_on": t.NextDueOn.Format("2006-01-02"), "active": t.Active, "created_by": t.CreatedBy}
}

type OccurrenceSQLiteRepository struct{}

func NewOccurrenceSQLiteRepository() *OccurrenceSQLiteRepository {
	return &OccurrenceSQLiteRepository{}
}

type occurrenceRow struct {
	ID         string         `db:"id"`
	GymID      string         `db:"gym_id"`
	TemplateID string         `db:"template_id"`
	Version    int            `db:"version"`
	CreatedAt  int64          `db:"created_at"`
	UpdatedAt  int64          `db:"updated_at"`
	DeletedAt  sql.NullInt64  `db:"deleted_at"`
	DueOn      string         `db:"due_on"`
	Amount     int64          `db:"expected_amount"`
	Category   string         `db:"category"`
	Payee      sql.NullString `db:"payee_name"`
	Method     string         `db:"payment_method"`
	Class      string         `db:"classification"`
	Status     string         `db:"status"`
	ExpenseID  sql.NullString `db:"expense_id"`
	ResolvedBy sql.NullString `db:"resolved_by"`
	ResolvedAt sql.NullInt64  `db:"resolved_at"`
	SkipReason sql.NullString `db:"skip_reason"`
}

const occurrenceCols = `id,gym_id,template_id,version,created_at,updated_at,deleted_at,due_on,expected_amount,category,payee_name,payment_method,classification,status,expense_id,resolved_by,resolved_at,skip_reason`

func (r *OccurrenceSQLiteRepository) CreateIfAbsent(tx shared.Transaction, o *recurring.Occurrence) (bool, error) {
	s := tx.(*shared.SqlxTransaction)
	res, err := s.Exec(context.Background(), `INSERT OR IGNORE INTO expense_occurrences(`+occurrenceCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, o.ID, o.GymID, o.TemplateID, o.Version, o.CreatedAt.UnixMilli(), o.UpdatedAt.UnixMilli(), nil, o.DueOn.Format("2006-01-02"), toCents(o.ExpectedAmount), o.Category, strPtrOrNil(o.PayeeName), o.PaymentMethod, o.Classification, o.Status, nil, nil, nil, nil)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if err = enqueueFinancial(s, "expense_occurrences", o.ID, o.GymID, o.Version, o.DeletedAt, occurrencePayload(o)); err != nil {
		return false, err
	}
	return true, nil
}
func (r *OccurrenceSQLiteRepository) Update(tx shared.Transaction, o *recurring.Occurrence, expected int) (*recurring.Occurrence, error) {
	s := tx.(*shared.SqlxTransaction)
	res, err := s.Exec(context.Background(), `UPDATE expense_occurrences SET version=?,updated_at=?,deleted_at=?,expected_amount=?,category=?,payee_name=?,payment_method=?,classification=?,status=?,expense_id=?,resolved_by=?,resolved_at=?,skip_reason=? WHERE gym_id=? AND id=? AND version=?`, o.Version, o.UpdatedAt.UnixMilli(), millisPtr(o.DeletedAt), toCents(o.ExpectedAmount), o.Category, strPtrOrNil(o.PayeeName), o.PaymentMethod, o.Classification, o.Status, uuidPtrString(o.ExpenseID), uuidPtrString(o.ResolvedBy), millisPtr(o.ResolvedAt), strPtrOrNil(o.SkipReason), o.GymID, o.ID, expected)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, expErrors.ErrVersionConflict
	}
	if err = enqueueFinancial(s, "expense_occurrences", o.ID, o.GymID, o.Version, o.DeletedAt, occurrencePayload(o)); err != nil {
		return nil, err
	}
	return o, nil
}
func (r *OccurrenceSQLiteRepository) GetByID(tx shared.Transaction, gymID, id uuid.UUID) (*recurring.Occurrence, error) {
	var row occurrenceRow
	err := tx.(*shared.SqlxTransaction).Get(context.Background(), &row, `SELECT `+occurrenceCols+` FROM expense_occurrences WHERE gym_id=? AND id=? AND deleted_at IS NULL`, gymID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, expErrors.ErrOccurrenceNotFound
	}
	if err != nil {
		return nil, err
	}
	return occurrenceFromRow(row), nil
}
func (r *OccurrenceSQLiteRepository) List(tx shared.Transaction, q expRepo.OccurrenceQuery) ([]*recurring.Occurrence, error) {
	limit := q.Limit
	if limit < 1 || limit > 500 {
		limit = 200
	}
	where := "gym_id=? AND deleted_at IS NULL"
	args := []any{q.GymID}
	if !q.From.IsZero() {
		where += " AND due_on>=?"
		args = append(args, q.From.Format("2006-01-02"))
	}
	if !q.To.IsZero() {
		where += " AND due_on<=?"
		args = append(args, q.To.Format("2006-01-02"))
	}
	if q.Status != "" {
		where += " AND status=?"
		args = append(args, q.Status)
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)
	var rows []occurrenceRow
	err := tx.(*shared.SqlxTransaction).Select(context.Background(), &rows, `SELECT `+occurrenceCols+` FROM expense_occurrences WHERE `+where+` ORDER BY CASE status WHEN 'pending' THEN 0 ELSE 1 END,due_on,id LIMIT ? OFFSET ?`, args...)
	out := make([]*recurring.Occurrence, len(rows))
	for i, x := range rows {
		out[i] = occurrenceFromRow(x)
	}
	return out, err
}

func (r *OccurrenceSQLiteRepository) Count(tx shared.Transaction, q expRepo.OccurrenceQuery) (int, error) {
	where := "gym_id=? AND deleted_at IS NULL"
	args := []any{q.GymID}
	if !q.From.IsZero() {
		where += " AND due_on>=?"
		args = append(args, q.From.Format("2006-01-02"))
	}
	if !q.To.IsZero() {
		where += " AND due_on<=?"
		args = append(args, q.To.Format("2006-01-02"))
	}
	if q.Status != "" {
		where += " AND status=?"
		args = append(args, q.Status)
	}
	var total int
	err := tx.(*shared.SqlxTransaction).Get(context.Background(), &total, `SELECT COUNT(*) FROM expense_occurrences WHERE `+where, args...)
	return total, err
}

func (r *OccurrenceSQLiteRepository) HasPendingByTemplate(tx shared.Transaction, gymID, templateID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.(*shared.SqlxTransaction).Get(context.Background(), &exists, `SELECT EXISTS(
		SELECT 1 FROM expense_occurrences WHERE gym_id=? AND template_id=? AND status='pending' AND deleted_at IS NULL
	)`, gymID, templateID)
	return exists, err
}

func (r *OccurrenceSQLiteRepository) UpdatePendingSnapshots(tx shared.Transaction, t *recurring.Template, _ time.Time) error {
	var rows []occurrenceRow
	err := tx.(*shared.SqlxTransaction).Select(context.Background(), &rows, `SELECT `+occurrenceCols+`
		FROM expense_occurrences WHERE gym_id=? AND template_id=? AND status='pending' AND deleted_at IS NULL
		ORDER BY due_on,id`, t.GymID, t.ID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		o := occurrenceFromRow(row)
		v := o.Version
		o.ExpectedAmount = t.ExpectedAmount
		o.Category = t.Category
		o.PayeeName = t.PayeeName
		o.PaymentMethod = t.PaymentMethod
		o.Classification = t.Classification
		o.Version++
		o.UpdatedAt = t.UpdatedAt
		if _, err = r.Update(tx, o, v); err != nil {
			return err
		}
	}
	return nil
}
func occurrenceFromRow(x occurrenceRow) *recurring.Occurrence {
	id, _ := uuid.Parse(x.ID)
	g, _ := uuid.Parse(x.GymID)
	t, _ := uuid.Parse(x.TemplateID)
	d, _ := time.Parse("2006-01-02", x.DueOn)
	o := &recurring.Occurrence{ID: id, GymID: g, TemplateID: t, Version: x.Version, DueOn: d, ExpectedAmount: fromCents(x.Amount), Category: x.Category, PaymentMethod: x.Method, Classification: x.Class, Status: x.Status, CreatedAt: time.UnixMilli(x.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(x.UpdatedAt).UTC()}
	if x.Payee.Valid {
		o.PayeeName = &x.Payee.String
	}
	o.ExpenseID = parseUUIDPtr(x.ExpenseID)
	o.ResolvedBy = parseUUIDPtr(x.ResolvedBy)
	if x.ResolvedAt.Valid {
		v := time.UnixMilli(x.ResolvedAt.Int64).UTC()
		o.ResolvedAt = &v
	}
	if x.SkipReason.Valid {
		o.SkipReason = &x.SkipReason.String
	}
	return o
}
func occurrencePayload(o *recurring.Occurrence) map[string]any {
	return map[string]any{"id": o.ID, "gym_id": o.GymID, "template_id": o.TemplateID, "version": o.Version, "created_at": o.CreatedAt.UnixMilli(), "updated_at": o.UpdatedAt.UnixMilli(), "deleted_at": millisPtr(o.DeletedAt), "due_on": o.DueOn.Format("2006-01-02"), "expected_amount": o.ExpectedAmount, "category": o.Category, "payee_name": o.PayeeName, "payment_method": o.PaymentMethod, "classification": o.Classification, "status": o.Status, "expense_id": o.ExpenseID, "resolved_by": o.ResolvedBy, "resolved_at": millisPtr(o.ResolvedAt), "skip_reason": o.SkipReason}
}

func enqueueFinancial(s *shared.SqlxTransaction, typ string, id, gym uuid.UUID, ver int, deleted *time.Time, p map[string]any) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	op := "upsert"
	if deleted != nil {
		op = "delete"
	}
	return s.EnqueueSync(context.Background(), typ, id.String(), op, b, ver)
}
func millisPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}
func datePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format("2006-01-02")
}
func uuidPtrString(v *uuid.UUID) any {
	if v == nil {
		return nil
	}
	return v.String()
}
func parseUUIDPtr(v sql.NullString) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	x, _ := uuid.Parse(v.String)
	return &x
}
