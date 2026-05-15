//go:build server

package repositories

import (
	"errors"
	"strings"
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
	return expenseFromModel(&row), nil
}

func (r *ExpensePostgresRepository) Update(tx sharedDomain.Transaction, e *expenseDomain.Expense) (*expenseDomain.Expense, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	e.UpdatedAt = time.Now().UTC()
	if err := gormTx.Model(&models.ExpenseModel{}).Where("id = ?", e.ID).
		Updates(map[string]any{
			"version":        e.Version,
			"updated_at":     e.UpdatedAt,
			"deleted_at":     e.DeletedAt,
			"expense_date":   e.ExpenseDate,
			"amount":         e.Amount,
			"category":       e.Category,
			"description":    e.Description,
			"payment_method": e.PaymentMethod,
		}).Error; err != nil {
		return nil, err
	}
	return e, nil
}

func (r *ExpensePostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*expenseDomain.Expense, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.ExpenseModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(expErrors.ErrExpenseNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return expenseFromModel(&row), nil
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
	return out, int(total), nil
}

// ListAggregates — total, cash/non-cash breakdown y categoría dominante
// sobre el set filtrado completo. Dos round-trips: una para los totales,
// otra para encontrar la categoría con mayor monto acumulado.
func (r *ExpensePostgresRepository) ListAggregates(tx sharedDomain.Transaction, q expRepo.ListQuery) (expRepo.ExpenseAggregates, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	base := buildExpenseFilterPg(gormTx.Model(&models.ExpenseModel{}), q)
	var totals struct {
		Total        float64
		CashTotal    float64
		NonCashTotal float64
	}
	if err := base.Session(&gorm.Session{}).Select(`
		COALESCE(SUM(amount), 0) AS total,
		COALESCE(SUM(CASE WHEN payment_method = 'cash' THEN amount ELSE 0 END), 0) AS cash_total,
		COALESCE(SUM(CASE WHEN payment_method <> 'cash' THEN amount ELSE 0 END), 0) AS non_cash_total
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
		Total:        totals.Total,
		CashTotal:    totals.CashTotal,
		NonCashTotal: totals.NonCashTotal,
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
	return out, nil
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
	if s := strings.TrimSpace(q.Search); s != "" {
		base = base.Where("LOWER(description) LIKE ?", "%"+strings.ToLower(s)+"%")
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
		return col + " " + direction + ", created_at DESC"
	}
	return col + " " + direction + ", expense_date DESC, created_at DESC"
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func expenseToModel(e *expenseDomain.Expense) models.ExpenseModel {
	return models.ExpenseModel{
		ID:            e.ID,
		GymID:         e.GymID,
		Version:       e.Version,
		CreatedAt:     e.CreatedAt,
		UpdatedAt:     e.UpdatedAt,
		DeletedAt:     e.DeletedAt,
		ExpenseDate:   e.ExpenseDate,
		Amount:        e.Amount,
		Category:      e.Category,
		Description:   e.Description,
		PaymentMethod: e.PaymentMethod,
		CreatedBy:     e.CreatedBy,
	}
}

func expenseFromModel(m *models.ExpenseModel) *expenseDomain.Expense {
	return &expenseDomain.Expense{
		ID:            m.ID,
		GymID:         m.GymID,
		Version:       m.Version,
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
		DeletedAt:     m.DeletedAt,
		ExpenseDate:   m.ExpenseDate,
		Amount:        m.Amount,
		Category:      m.Category,
		Description:   m.Description,
		PaymentMethod: m.PaymentMethod,
		CreatedBy:     m.CreatedBy,
	}
}
