//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	expenseDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ExpensePostgresRepository struct{}

func NewExpensePostgresRepository() *ExpensePostgresRepository {
	return &ExpensePostgresRepository{}
}

func (r *ExpensePostgresRepository) Create(tx sharedDomain.Transaction, e *expenseDomain.Expense) (*expenseDomain.Expense, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := expenseToModel(e)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	if err := emitExpenseToSync(gormTx, e); err != nil {
		return nil, err
	}
	return expenseFromModel(&row), nil
}

func (r *ExpensePostgresRepository) Update(tx sharedDomain.Transaction, e *expenseDomain.Expense) (*expenseDomain.Expense, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	e.UpdatedAt = time.Now().UTC()
	res := gormTx.Model(&models.ExpenseModel{}).Where("gym_id = ? AND id = ? AND version = ?", e.GymID, e.ID, e.Version-1).
		Updates(map[string]any{
			"version":        e.Version,
			"updated_at":     e.UpdatedAt,
			"deleted_at":     e.DeletedAt,
			"expense_date":   e.ExpenseDate,
			"amount":         e.Amount,
			"category":       e.Category,
			"payee_name":     e.PayeeName,
			"description":    e.Description,
			"reference":      e.Reference,
			"payment_method": e.PaymentMethod,
			"paid_from":      e.PaidFrom,
			"classification": e.Classification, "source": e.Source, "recurring_occurrence_id": e.RecurringOccurrenceID, "cash_movement_id": e.CashMovementID,
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, expErrors.ErrVersionConflict
	}
	if err := emitExpenseToSync(gormTx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// emitExpenseToSync mirrors every canonical cloud mutation in the same
// transaction. Payload money stays in pesos; the sidecar apply edge converts
// it to integer cents. A soft delete is a durable tombstone, not an omitted row.
func emitExpenseToSync(g *gorm.DB, e *expenseDomain.Expense) error {
	payload, err := json.Marshal(map[string]any{
		"id": e.ID.String(), "gym_id": e.GymID.String(), "version": e.Version,
		"created_at": e.CreatedAt.UnixMilli(), "updated_at": e.UpdatedAt.UnixMilli(),
		"deleted_at": e.DeletedAt, "expense_date": e.ExpenseDate.Format("2006-01-02"),
		"amount": e.Amount, "category": e.Category, "description": e.Description,
		"payee_name": e.PayeeName, "reference": e.Reference, "classification": e.Classification, "source": e.Source, "recurring_occurrence_id": e.RecurringOccurrenceID, "cash_movement_id": e.CashMovementID,
		"payment_method": e.PaymentMethod, "paid_from": e.PaidFrom, "created_by": e.CreatedBy.String(),
	})
	if err != nil {
		return err
	}
	return g.Exec(`
		INSERT INTO sync_entities
		 (gym_id, entity_type, entity_id, version, payload, server_updated_at, deleted_at)
		VALUES (?, 'expenses', ?, ?, ?::jsonb, NOW(), ?)
		ON CONFLICT (gym_id, entity_type, entity_id) DO UPDATE SET
		 version=EXCLUDED.version, payload=EXCLUDED.payload,
		 server_updated_at=NOW(), deleted_at=EXCLUDED.deleted_at`,
		e.GymID, e.ID, e.Version, string(payload), e.DeletedAt).Error
}

func (r *ExpensePostgresRepository) GetByID(tx sharedDomain.Transaction, gymID, id uuid.UUID) (*expenseDomain.Expense, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.ExpenseModel
	err := gormTx.Where("gym_id = ? AND id = ? AND deleted_at IS NULL", gymID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(expErrors.ErrExpenseNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	e := expenseFromModel(&row)
	if err := hydrateExpenseCashDrawersPostgres(gormTx, gymID, []*expenseDomain.Expense{e}); err != nil {
		return nil, err
	}
	return e, nil
}

func (r *ExpensePostgresRepository) List(tx sharedDomain.Transaction, q expRepo.ListQuery) ([]*expenseDomain.Expense, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	page, pageSize := normalizePage(q.Page, q.PageSize)
	base := buildExpenseFilterPg(gormTx.Model(&models.ExpenseModel{}), q)
	var total int64
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.ExpenseModel
	if err := base.Order(sortClausePostgres(q.Sort, q.Direction)).
		Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*expenseDomain.Expense, len(rows))
	for i := range rows {
		out[i] = expenseFromModel(&rows[i])
	}
	if err := hydrateExpenseCashDrawersPostgres(gormTx, q.GymID, out); err != nil {
		return nil, 0, err
	}
	return out, int(total), nil
}

// ListAggregates — total, cash/non-cash breakdown y categoría dominante
// sobre el set filtrado completo. Dos round-trips: una para los totales,
// otra para encontrar la categoría con mayor monto acumulado.
func (r *ExpensePostgresRepository) ListAggregates(tx sharedDomain.Transaction, q expRepo.ListQuery) (expRepo.ExpenseAggregates, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	base := buildExpenseFilterPg(gormTx.Model(&models.ExpenseModel{}), q)
	var totals struct {
		Total         float64
		CashTotal     float64
		NonCashTotal  float64
		FixedTotal    float64
		VariableTotal float64
	}
	if err := base.Session(&gorm.Session{}).Select(`
		COALESCE(SUM(amount), 0) AS total,
		COALESCE(SUM(CASE WHEN payment_method = 'cash' THEN amount ELSE 0 END), 0) AS cash_total,
		COALESCE(SUM(CASE WHEN payment_method <> 'cash' THEN amount ELSE 0 END), 0) AS non_cash_total,
		COALESCE(SUM(CASE WHEN classification = 'fixed' THEN amount ELSE 0 END), 0) AS fixed_total,
		COALESCE(SUM(CASE WHEN classification = 'variable' THEN amount ELSE 0 END), 0) AS variable_total
	`).Scan(&totals).Error; err != nil {
		return expRepo.ExpenseAggregates{}, err
	}

	type catRow struct {
		Category string
		Total    float64
	}
	var cats []catRow
	if err := buildExpenseFilterPg(gormTx.Model(&models.ExpenseModel{}), q).
		Session(&gorm.Session{}).
		Select("category, COALESCE(SUM(amount), 0) AS total").
		Group("category").Order("total DESC").Limit(1).Scan(&cats).Error; err != nil {
		return expRepo.ExpenseAggregates{}, err
	}
	out := expRepo.ExpenseAggregates{
		Total:         totals.Total,
		CashTotal:     totals.CashTotal,
		NonCashTotal:  totals.NonCashTotal,
		FixedTotal:    totals.FixedTotal,
		VariableTotal: totals.VariableTotal,
	}
	if len(cats) > 0 {
		out.DominantCategory = cats[0].Category
		out.DominantCatTotal = cats[0].Total
	}
	return out, nil
}

// ListByDate — todos los gastos cuyo expense_date coincide con el día
// indicado, ordenados por created_at DESC (último capturado arriba). Sin
// paginación: el corte del día sólo necesita los del día.
func (r *ExpensePostgresRepository) ListByDate(tx sharedDomain.Transaction, gymID uuid.UUID, day time.Time) ([]*expenseDomain.Expense, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	dateStr := day.UTC().Format("2006-01-02")
	var rows []models.ExpenseModel
	if err := gormTx.
		Where("gym_id = ? AND deleted_at IS NULL AND expense_date = ?", gymID, dateStr).
		Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*expenseDomain.Expense, len(rows))
	for i := range rows {
		out[i] = expenseFromModel(&rows[i])
	}
	if err := hydrateExpenseCashDrawersPostgres(gormTx, gymID, out); err != nil {
		return nil, err
	}
	return out, nil
}

func hydrateExpenseCashDrawersPostgres(g *gorm.DB, gymID uuid.UUID, expenses []*expenseDomain.Expense) error {
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
	var rows []struct {
		ID           uuid.UUID `gorm:"column:id"`
		CashDrawerID uuid.UUID `gorm:"column:cash_drawer_id"`
	}
	if err := g.Raw(`SELECT id,COALESCE(cash_drawer_id,gym_id) AS cash_drawer_id
		FROM cash_movements WHERE gym_id=? AND deleted_at IS NULL AND id IN ?`, gymID, ids).Scan(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if e := byMovement[row.ID]; e != nil {
			d := row.CashDrawerID
			e.CashDrawerID = &d
		}
	}
	return nil
}

func buildExpenseFilterPg(base *gorm.DB, q expRepo.ListQuery) *gorm.DB {
	base = base.Where("gym_id = ? AND deleted_at IS NULL", q.GymID)
	if q.From != nil {
		base = base.Where("expense_date >= ?", q.From.Format("2006-01-02"))
	}
	if q.To != nil {
		base = base.Where("expense_date <= ?", q.To.Format("2006-01-02"))
	}
	if q.Category != "" {
		base = base.Where("category = ?", q.Category)
	}
	if q.PaymentMethod != "" {
		base = base.Where("payment_method = ?", q.PaymentMethod)
	}
	if q.Source != "" {
		base = base.Where("source = ?", q.Source)
	}
	if q.Classification != "" {
		base = base.Where("classification = ?", q.Classification)
	}
	if s := normalizeExpenseSearch(q.Search); s != "" {
		pattern := "%" + escapeLike(s) + "%"
		clause := "(" + postgresNormalizedSearchColumn("payee_name") + " LIKE ? ESCAPE '!' OR " + postgresNormalizedSearchColumn("description") + " LIKE ? ESCAPE '!' OR " + postgresNormalizedSearchColumn("reference") + " LIKE ? ESCAPE '!')"
		base = base.Where(clause, pattern, pattern, pattern)
	}
	return base
}

func sortClausePostgres(sort, dir string) string {
	col := "expense_date"
	switch sort {
	case expRepo.SortAmount:
		col = "amount"
	case expRepo.SortCategory:
		col = "category"
	case expRepo.SortMethod:
		col = "payment_method"
	}
	// Default desc para fecha (más reciente arriba), asc para el resto.
	direction := "ASC"
	if dir == expRepo.SortDirDesc || (dir == "" && (sort == "" || sort == expRepo.SortDate)) {
		direction = "DESC"
	}
	if dir == expRepo.SortDirAsc {
		direction = "ASC"
	}
	// Tiebreaker por created_at desc para estabilidad cuando hay empate.
	if sort == expRepo.SortDate || sort == "" {
		return col + " " + direction + ", created_at DESC, id ASC"
	}
	return col + " " + direction + ", expense_date DESC, created_at DESC, id ASC"
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func expenseToModel(e *expenseDomain.Expense) models.ExpenseModel {
	return models.ExpenseModel{
		ID:             e.ID,
		GymID:          e.GymID,
		Version:        e.Version,
		CreatedAt:      e.CreatedAt,
		UpdatedAt:      e.UpdatedAt,
		DeletedAt:      e.DeletedAt,
		ExpenseDate:    e.ExpenseDate,
		Amount:         e.Amount,
		Category:       e.Category,
		PayeeName:      e.PayeeName,
		Description:    e.Description,
		Reference:      e.Reference,
		PaymentMethod:  e.PaymentMethod,
		PaidFrom:       e.PaidFrom,
		Classification: e.Classification, Source: e.Source, RecurringOccurrenceID: e.RecurringOccurrenceID, CashMovementID: e.CashMovementID,
		CreatedBy: e.CreatedBy,
	}
}

func expenseFromModel(m *models.ExpenseModel) *expenseDomain.Expense {
	return &expenseDomain.Expense{
		ID:             m.ID,
		GymID:          m.GymID,
		Version:        m.Version,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
		DeletedAt:      m.DeletedAt,
		ExpenseDate:    m.ExpenseDate,
		PaidOn:         m.ExpenseDate,
		Amount:         m.Amount,
		Category:       m.Category,
		PayeeName:      m.PayeeName,
		Description:    m.Description,
		Reference:      m.Reference,
		PaymentMethod:  m.PaymentMethod,
		PaidFrom:       m.PaidFrom,
		Classification: m.Classification, Source: m.Source, RecurringOccurrenceID: m.RecurringOccurrenceID, CashMovementID: m.CashMovementID,
		CreatedBy: m.CreatedBy,
	}
}
