//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	cashDomain "github.com/cuadra/cuadra-core/src/modules/expenses/domain/cashmovement"
	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	recurring "github.com/cuadra/cuadra-core/src/modules/expenses/domain/recurring"
	expRepo "github.com/cuadra/cuadra-core/src/modules/expenses/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

type CashDayLockPostgres struct{}

func NewCashDayLockPostgres() *CashDayLockPostgres { return &CashDayLockPostgres{} }
func (*CashDayLockPostgres) IsClosed(tx shared.Transaction, gymID uuid.UUID, day time.Time) (bool, error) {
	var n int64
	err := tx.(*shared.GormTransaction).Tx.Table("cash_close_events").Where("gym_id=? AND close_date=? AND deleted_at IS NULL", gymID, day.Format("2006-01-02")).Count(&n).Error
	return n > 0, err
}

type cashPG struct {
	ID, GymID            uuid.UUID
	CashDrawerID         *uuid.UUID `gorm:"column:cash_drawer_id"`
	Version              int
	CreatedAt, UpdatedAt time.Time
	DeletedAt            *time.Time
	MovementOn           time.Time `gorm:"column:movement_on"`
	Amount               float64
	MovementType, Reason string
	OperatorID           uuid.UUID
	ExpenseID            *uuid.UUID
	ClassificationStatus string
}

func (cashPG) TableName() string { return "cash_movements" }

type CashMovementPostgresRepository struct{}

func NewCashMovementPostgresRepository() *CashMovementPostgresRepository {
	return &CashMovementPostgresRepository{}
}
func (r *CashMovementPostgresRepository) Create(tx shared.Transaction, m *cashDomain.CashMovement) (*cashDomain.CashMovement, error) {
	g := tx.(*shared.GormTransaction).Tx
	row := cashToPG(m)
	if err := g.Create(&row).Error; err != nil {
		return nil, err
	}
	if err := emitFinancial(g, "cash_movements", m.ID, m.GymID, m.Version, m.DeletedAt, cashPayloadPG(m)); err != nil {
		return nil, err
	}
	return m, nil
}
func (r *CashMovementPostgresRepository) Update(tx shared.Transaction, m *cashDomain.CashMovement, expected int) (*cashDomain.CashMovement, error) {
	g := tx.(*shared.GormTransaction).Tx
	res := g.Model(&cashPG{}).Where("gym_id=? AND id=? AND version=?", m.GymID, m.ID, expected).Updates(map[string]any{"version": m.Version, "updated_at": m.UpdatedAt, "deleted_at": m.DeletedAt, "movement_on": m.MovementOn, "amount": m.Amount, "movement_type": m.MovementType, "reason": m.Reason, "expense_id": m.ExpenseID, "classification_status": m.ClassificationStatus, "cash_drawer_id": cashDrawerIDPG(m)})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, expErrors.ErrVersionConflict
	}
	if err := emitFinancial(g, "cash_movements", m.ID, m.GymID, m.Version, m.DeletedAt, cashPayloadPG(m)); err != nil {
		return nil, err
	}
	return m, nil
}
func (r *CashMovementPostgresRepository) GetByID(tx shared.Transaction, gymID, id uuid.UUID) (*cashDomain.CashMovement, error) {
	var row cashPG
	err := tx.(*shared.GormTransaction).Tx.Where("gym_id=? AND id=? AND deleted_at IS NULL", gymID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, expErrors.ErrCashMovementNotFound
	}
	if err != nil {
		return nil, err
	}
	return cashFromPG(row), nil
}
func (r *CashMovementPostgresRepository) GetByExpenseID(tx shared.Transaction, gymID, expenseID uuid.UUID) (*cashDomain.CashMovement, error) {
	var row cashPG
	err := tx.(*shared.GormTransaction).Tx.Where("gym_id=? AND expense_id=? AND deleted_at IS NULL", gymID, expenseID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, expErrors.ErrCashMovementNotFound
	}
	if err != nil {
		return nil, err
	}
	return cashFromPG(row), nil
}
func (r *CashMovementPostgresRepository) List(tx shared.Transaction, q expRepo.CashMovementListQuery) ([]*cashDomain.CashMovement, int, error) {
	page, pageSize := normalizePage(q.Page, q.PageSize)
	base := tx.(*shared.GormTransaction).Tx.Model(&cashPG{}).
		Where("gym_id=? AND deleted_at IS NULL", q.GymID)
	if q.From != nil {
		base = base.Where("movement_on>=?", q.From.Format("2006-01-02"))
	}
	if q.To != nil {
		base = base.Where("movement_on<=?", q.To.Format("2006-01-02"))
	}
	switch q.Status {
	case "pending", cashDomain.Unclassified:
		base = base.Where("movement_type='cash_out' AND classification_status='unclassified'")
	case "classified":
		base = base.Where("classification_status<>'unclassified'")
	case cashDomain.AsExpense, cashDomain.AsInventoryPurchase, cashDomain.NonOperating:
		base = base.Where("classification_status=?", q.Status)
	}
	var total int64
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []cashPG
	err := base.Order("movement_on DESC,created_at DESC,id DESC").
		Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error
	return cashRowsPG(rows), int(total), err
}
func (r *CashMovementPostgresRepository) ListByDate(tx shared.Transaction, gymID uuid.UUID, day time.Time) ([]*cashDomain.CashMovement, error) {
	var rows []cashPG
	err := tx.(*shared.GormTransaction).Tx.Where("gym_id=? AND movement_on=? AND deleted_at IS NULL", gymID, day.Format("2006-01-02")).Order("created_at,id").Find(&rows).Error
	return cashRowsPG(rows), err
}
func (r *CashMovementPostgresRepository) ListUnclassified(tx shared.Transaction, gymID uuid.UUID, limit int) ([]*cashDomain.CashMovement, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	var rows []cashPG
	err := tx.(*shared.GormTransaction).Tx.Where("gym_id=? AND movement_type='cash_out' AND classification_status='unclassified' AND deleted_at IS NULL", gymID).Order("movement_on,created_at,id").Limit(limit).Find(&rows).Error
	return cashRowsPG(rows), err
}
func cashToPG(m *cashDomain.CashMovement) cashPG {
	return cashPG{ID: m.ID, GymID: m.GymID, CashDrawerID: cashDrawerIDPG(m), Version: m.Version,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, DeletedAt: m.DeletedAt,
		MovementOn: m.MovementOn, Amount: m.Amount, MovementType: m.MovementType,
		Reason: m.Reason, OperatorID: m.OperatorID, ExpenseID: m.ExpenseID,
		ClassificationStatus: m.ClassificationStatus}
}
func cashFromPG(x cashPG) *cashDomain.CashMovement {
	drawerID := x.GymID
	if x.CashDrawerID != nil && *x.CashDrawerID != uuid.Nil {
		drawerID = *x.CashDrawerID
	}
	return &cashDomain.CashMovement{ID: x.ID, GymID: x.GymID, CashDrawerID: drawerID,
		Version: x.Version, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt,
		DeletedAt: x.DeletedAt, MovementOn: x.MovementOn, Amount: x.Amount,
		MovementType: x.MovementType, Reason: x.Reason, OperatorID: x.OperatorID,
		ExpenseID: x.ExpenseID, ClassificationStatus: x.ClassificationStatus}
}
func cashRowsPG(rows []cashPG) []*cashDomain.CashMovement {
	out := make([]*cashDomain.CashMovement, len(rows))
	for i, x := range rows {
		out[i] = cashFromPG(x)
	}
	return out
}
func cashPayloadPG(m *cashDomain.CashMovement) map[string]any {
	return map[string]any{"id": m.ID, "gym_id": m.GymID, "version": m.Version, "created_at": m.CreatedAt.UnixMilli(), "updated_at": m.UpdatedAt.UnixMilli(), "deleted_at": m.DeletedAt, "movement_on": m.MovementOn.Format("2006-01-02"), "amount": m.Amount, "movement_type": m.MovementType, "reason": m.Reason, "operator_id": m.OperatorID, "expense_id": m.ExpenseID, "classification_status": m.ClassificationStatus, "cash_drawer_id": cashDrawerIDPG(m)}
}

func cashDrawerIDPG(m *cashDomain.CashMovement) *uuid.UUID {
	drawerID := m.CashDrawerID
	if drawerID == uuid.Nil {
		drawerID = m.GymID
	}
	return &drawerID
}

type templatePG struct {
	ID, GymID                 uuid.UUID
	Version                   int
	CreatedAt, UpdatedAt      time.Time
	DeletedAt                 *time.Time
	Name                      string
	PayeeName                 *string
	Category                  string
	ExpectedAmount            float64
	UsualPaymentMethod        string
	Classification, Frequency string
	StartsOn                  time.Time
	EndsOn                    *time.Time
	NextDueOn                 time.Time
	Active                    bool
	CreatedBy                 uuid.UUID
}

func (templatePG) TableName() string { return "recurring_expense_templates" }

type RecurringTemplatePostgresRepository struct{}

func NewRecurringTemplatePostgresRepository() *RecurringTemplatePostgresRepository {
	return &RecurringTemplatePostgresRepository{}
}
func (r *RecurringTemplatePostgresRepository) Create(tx shared.Transaction, t *recurring.Template) (*recurring.Template, error) {
	g := tx.(*shared.GormTransaction).Tx
	row := templateToPG(t)
	if err := g.Create(&row).Error; err != nil {
		return nil, err
	}
	if err := emitFinancial(g, "recurring_expense_templates", t.ID, t.GymID, t.Version, t.DeletedAt, templatePayloadPG(t)); err != nil {
		return nil, err
	}
	return t, nil
}
func (r *RecurringTemplatePostgresRepository) Update(tx shared.Transaction, t *recurring.Template, expected int) (*recurring.Template, error) {
	g := tx.(*shared.GormTransaction).Tx
	res := g.Model(&templatePG{}).Where("gym_id=? AND id=? AND version=?", t.GymID, t.ID, expected).Updates(map[string]any{"version": t.Version, "updated_at": t.UpdatedAt, "deleted_at": t.DeletedAt, "name": t.Name, "payee_name": t.PayeeName, "category": t.Category, "expected_amount": t.ExpectedAmount, "usual_payment_method": t.PaymentMethod, "classification": t.Classification, "frequency": t.Frequency, "starts_on": t.StartsOn, "ends_on": t.EndsOn, "next_due_on": t.NextDueOn, "active": t.Active})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, expErrors.ErrVersionConflict
	}
	if err := emitFinancial(g, "recurring_expense_templates", t.ID, t.GymID, t.Version, t.DeletedAt, templatePayloadPG(t)); err != nil {
		return nil, err
	}
	return t, nil
}
func (r *RecurringTemplatePostgresRepository) GetByID(tx shared.Transaction, gymID, id uuid.UUID) (*recurring.Template, error) {
	var row templatePG
	err := tx.(*shared.GormTransaction).Tx.Where("gym_id=? AND id=? AND deleted_at IS NULL", gymID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, expErrors.ErrTemplateNotFound
	}
	if err != nil {
		return nil, err
	}
	return templateFromPG(row), nil
}
func (r *RecurringTemplatePostgresRepository) List(tx shared.Transaction, gymID uuid.UUID, include bool) ([]*recurring.Template, error) {
	q := tx.(*shared.GormTransaction).Tx.Where("gym_id=? AND deleted_at IS NULL", gymID)
	if !include {
		q = q.Where("active=true")
	}
	var rows []templatePG
	err := q.Order("active DESC,next_due_on,name,id").Find(&rows).Error
	out := make([]*recurring.Template, len(rows))
	for i, x := range rows {
		out[i] = templateFromPG(x)
	}
	return out, err
}
func templateToPG(t *recurring.Template) templatePG {
	return templatePG{t.ID, t.GymID, t.Version, t.CreatedAt, t.UpdatedAt, t.DeletedAt, t.Name, t.PayeeName, t.Category, t.ExpectedAmount, t.PaymentMethod, t.Classification, t.Frequency, t.StartsOn, t.EndsOn, t.NextDueOn, t.Active, t.CreatedBy}
}
func templateFromPG(x templatePG) *recurring.Template {
	return &recurring.Template{ID: x.ID, GymID: x.GymID, Version: x.Version, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt, DeletedAt: x.DeletedAt, Name: x.Name, PayeeName: x.PayeeName, Category: x.Category, ExpectedAmount: x.ExpectedAmount, PaymentMethod: x.UsualPaymentMethod, Classification: x.Classification, Frequency: x.Frequency, StartsOn: x.StartsOn, EndsOn: x.EndsOn, NextDueOn: x.NextDueOn, Active: x.Active, CreatedBy: x.CreatedBy}
}
func templatePayloadPG(t *recurring.Template) map[string]any {
	return map[string]any{"id": t.ID, "gym_id": t.GymID, "version": t.Version, "created_at": t.CreatedAt.UnixMilli(), "updated_at": t.UpdatedAt.UnixMilli(), "deleted_at": t.DeletedAt, "name": t.Name, "payee_name": t.PayeeName, "category": t.Category, "expected_amount": t.ExpectedAmount, "usual_payment_method": t.PaymentMethod, "classification": t.Classification, "frequency": t.Frequency, "starts_on": t.StartsOn.Format("2006-01-02"), "ends_on": datePtrPG(t.EndsOn), "next_due_on": t.NextDueOn.Format("2006-01-02"), "active": t.Active, "created_by": t.CreatedBy}
}

type occurrencePG struct {
	ID, GymID, TemplateID uuid.UUID
	Version               int
	CreatedAt, UpdatedAt  time.Time
	DeletedAt             *time.Time
	DueOn                 time.Time
	ExpectedAmount        float64
	Category              string
	PayeeName             *string
	PaymentMethod         string
	Classification        string
	Status                string
	ExpenseID, ResolvedBy *uuid.UUID
	ResolvedAt            *time.Time
	SkipReason            *string
}

func (occurrencePG) TableName() string { return "expense_occurrences" }

type OccurrencePostgresRepository struct{}

func NewOccurrencePostgresRepository() *OccurrencePostgresRepository {
	return &OccurrencePostgresRepository{}
}
func (r *OccurrencePostgresRepository) CreateIfAbsent(tx shared.Transaction, o *recurring.Occurrence) (bool, error) {
	g := tx.(*shared.GormTransaction).Tx
	row := occurrenceToPG(o)
	res := g.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "gym_id"}, {Name: "template_id"}, {Name: "due_on"}}, DoNothing: true}).Create(&row)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	if err := emitFinancial(g, "expense_occurrences", o.ID, o.GymID, o.Version, o.DeletedAt, occurrencePayloadPG(o)); err != nil {
		return false, err
	}
	return true, nil
}
func (r *OccurrencePostgresRepository) Update(tx shared.Transaction, o *recurring.Occurrence, expected int) (*recurring.Occurrence, error) {
	g := tx.(*shared.GormTransaction).Tx
	res := g.Model(&occurrencePG{}).Where("gym_id=? AND id=? AND version=?", o.GymID, o.ID, expected).Updates(map[string]any{"version": o.Version, "updated_at": o.UpdatedAt, "deleted_at": o.DeletedAt, "expected_amount": o.ExpectedAmount, "category": o.Category, "payee_name": o.PayeeName, "payment_method": o.PaymentMethod, "classification": o.Classification, "status": o.Status, "expense_id": o.ExpenseID, "resolved_by": o.ResolvedBy, "resolved_at": o.ResolvedAt, "skip_reason": o.SkipReason})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, expErrors.ErrVersionConflict
	}
	if err := emitFinancial(g, "expense_occurrences", o.ID, o.GymID, o.Version, o.DeletedAt, occurrencePayloadPG(o)); err != nil {
		return nil, err
	}
	return o, nil
}
func (r *OccurrencePostgresRepository) GetByID(tx shared.Transaction, gymID, id uuid.UUID) (*recurring.Occurrence, error) {
	var row occurrencePG
	err := tx.(*shared.GormTransaction).Tx.Where("gym_id=? AND id=? AND deleted_at IS NULL", gymID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, expErrors.ErrOccurrenceNotFound
	}
	if err != nil {
		return nil, err
	}
	return occurrenceFromPG(row), nil
}
func (r *OccurrencePostgresRepository) List(tx shared.Transaction, q expRepo.OccurrenceQuery) ([]*recurring.Occurrence, error) {
	limit := q.Limit
	if limit < 1 || limit > 500 {
		limit = 200
	}
	x := tx.(*shared.GormTransaction).Tx.Where("gym_id=? AND deleted_at IS NULL", q.GymID)
	if !q.From.IsZero() {
		x = x.Where("due_on>=?", q.From.Format("2006-01-02"))
	}
	if !q.To.IsZero() {
		x = x.Where("due_on<=?", q.To.Format("2006-01-02"))
	}
	if q.Status != "" {
		x = x.Where("status=?", q.Status)
	}
	var rows []occurrencePG
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	err := x.Order("CASE status WHEN 'pending' THEN 0 ELSE 1 END,due_on,id").Limit(limit).Offset(offset).Find(&rows).Error
	out := make([]*recurring.Occurrence, len(rows))
	for i, v := range rows {
		out[i] = occurrenceFromPG(v)
	}
	return out, err
}

func (r *OccurrencePostgresRepository) Count(tx shared.Transaction, q expRepo.OccurrenceQuery) (int, error) {
	x := tx.(*shared.GormTransaction).Tx.Model(&occurrencePG{}).
		Where("gym_id=? AND deleted_at IS NULL", q.GymID)
	if !q.From.IsZero() {
		x = x.Where("due_on>=?", q.From.Format("2006-01-02"))
	}
	if !q.To.IsZero() {
		x = x.Where("due_on<=?", q.To.Format("2006-01-02"))
	}
	if q.Status != "" {
		x = x.Where("status=?", q.Status)
	}
	var total int64
	err := x.Count(&total).Error
	return int(total), err
}

func (r *OccurrencePostgresRepository) HasPendingByTemplate(tx shared.Transaction, gymID, templateID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.(*shared.GormTransaction).Tx.Raw(`SELECT EXISTS(
		SELECT 1 FROM expense_occurrences WHERE gym_id=? AND template_id=? AND status='pending' AND deleted_at IS NULL
	)`, gymID, templateID).Scan(&exists).Error
	return exists, err
}

func (r *OccurrencePostgresRepository) UpdatePendingSnapshots(tx shared.Transaction, t *recurring.Template, _ time.Time) error {
	var stored []occurrencePG
	err := tx.(*shared.GormTransaction).Tx.Where("gym_id=? AND template_id=? AND status=? AND deleted_at IS NULL", t.GymID, t.ID, recurring.Pending).
		Order("due_on,id").Find(&stored).Error
	if err != nil {
		return err
	}
	for _, row := range stored {
		o := occurrenceFromPG(row)
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
func occurrenceToPG(o *recurring.Occurrence) occurrencePG {
	return occurrencePG{o.ID, o.GymID, o.TemplateID, o.Version, o.CreatedAt, o.UpdatedAt, o.DeletedAt, o.DueOn, o.ExpectedAmount, o.Category, o.PayeeName, o.PaymentMethod, o.Classification, o.Status, o.ExpenseID, o.ResolvedBy, o.ResolvedAt, o.SkipReason}
}
func occurrenceFromPG(x occurrencePG) *recurring.Occurrence {
	return &recurring.Occurrence{ID: x.ID, GymID: x.GymID, TemplateID: x.TemplateID, Version: x.Version, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt, DeletedAt: x.DeletedAt, DueOn: x.DueOn, ExpectedAmount: x.ExpectedAmount, Category: x.Category, PayeeName: x.PayeeName, PaymentMethod: x.PaymentMethod, Classification: x.Classification, Status: x.Status, ExpenseID: x.ExpenseID, ResolvedBy: x.ResolvedBy, ResolvedAt: x.ResolvedAt, SkipReason: x.SkipReason}
}
func occurrencePayloadPG(o *recurring.Occurrence) map[string]any {
	return map[string]any{"id": o.ID, "gym_id": o.GymID, "template_id": o.TemplateID, "version": o.Version, "created_at": o.CreatedAt.UnixMilli(), "updated_at": o.UpdatedAt.UnixMilli(), "deleted_at": o.DeletedAt, "due_on": o.DueOn.Format("2006-01-02"), "expected_amount": o.ExpectedAmount, "category": o.Category, "payee_name": o.PayeeName, "payment_method": o.PaymentMethod, "classification": o.Classification, "status": o.Status, "expense_id": o.ExpenseID, "resolved_by": o.ResolvedBy, "resolved_at": o.ResolvedAt, "skip_reason": o.SkipReason}
}

func emitFinancial(g *gorm.DB, typ string, id, gym uuid.UUID, ver int, deleted *time.Time, p map[string]any) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return g.Exec(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at,deleted_at) VALUES(?,?,?,?,?::jsonb,NOW(),?) ON CONFLICT(gym_id,entity_type,entity_id) DO UPDATE SET version=EXCLUDED.version,payload=EXCLUDED.payload,server_updated_at=NOW(),deleted_at=EXCLUDED.deleted_at`, gym, typ, id, ver, string(b), deleted).Error
}
func datePtrPG(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format("2006-01-02")
}
