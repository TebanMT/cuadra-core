//go:build server

package repositories

import (
	"time"

	"github.com/google/uuid"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CashClosePostgresReader struct{}

func NewCashClosePostgresReader() *CashClosePostgresReader { return &CashClosePostgresReader{} }

// Aggregate runs the per-day rollup. Refunds (negative amounts) are NOT folded
// into ByMethod / ByConcept — they're surfaced in RefundTotal so the operator
// can see the gross take separately.
func (r *CashClosePostgresReader) Aggregate(tx sharedDomain.Transaction, q billingRepo.CashCloseQuery) (*billingRepo.CashCloseTotals, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	dateStr := q.Date.UTC().Format("2006-01-02")

	type methodRow struct {
		PaymentMethod string
		Total         float64
	}
	var methodRows []methodRow
	if err := gormTx.Model(&models.PaymentModel{}).
		Select("payment_method, SUM(amount) AS total").
		Where("gym_id = ? AND payment_date = ? AND concept <> ? AND deleted_at IS NULL",
			q.GymID, dateStr, paymentDomain.ConceptRefund).
		Group("payment_method").Scan(&methodRows).Error; err != nil {
		return nil, err
	}

	type conceptRow struct {
		Concept string
		Total   float64
	}
	var conceptRows []conceptRow
	if err := gormTx.Model(&models.PaymentModel{}).
		Select("concept, SUM(amount) AS total").
		Where("gym_id = ? AND payment_date = ? AND concept <> ? AND deleted_at IS NULL",
			q.GymID, dateStr, paymentDomain.ConceptRefund).
		Group("concept").Scan(&conceptRows).Error; err != nil {
		return nil, err
	}

	var refundTotal float64
	if err := gormTx.Model(&models.PaymentModel{}).
		Select("COALESCE(SUM(amount), 0) AS total").
		Where("gym_id = ? AND payment_date = ? AND concept = ? AND deleted_at IS NULL",
			q.GymID, dateStr, paymentDomain.ConceptRefund).
		Scan(&refundTotal).Error; err != nil {
		return nil, err
	}

	type opRow struct {
		OperatorID uuid.UUID
		Total      float64
		Cnt        int
	}
	var opRows []opRow
	if err := gormTx.Model(&models.PaymentModel{}).
		Select("operator_id, SUM(amount) AS total, COUNT(*) AS cnt").
		Where("gym_id = ? AND payment_date = ? AND concept <> ? AND deleted_at IS NULL",
			q.GymID, dateStr, paymentDomain.ConceptRefund).
		Group("operator_id").Scan(&opRows).Error; err != nil {
		return nil, err
	}

	out := &billingRepo.CashCloseTotals{
		ByMethod:    map[string]float64{},
		ByConcept:   map[string]float64{},
		ByOperator:  make([]billingRepo.OperatorTotal, 0, len(opRows)),
		RefundTotal: refundTotal,
	}
	for _, m := range methodRows {
		out.ByMethod[m.PaymentMethod] = m.Total
		out.GrandTotal += m.Total
	}
	for _, c := range conceptRows {
		out.ByConcept[c.Concept] = c.Total
	}
	for _, o := range opRows {
		out.ByOperator = append(out.ByOperator, billingRepo.OperatorTotal{
			OperatorID: o.OperatorID,
			Total:      o.Total,
			PaymentsN:  o.Cnt,
		})
	}
	return out, nil
}

type CashCloseEventPostgresRepository struct{}

func NewCashCloseEventPostgresRepository() *CashCloseEventPostgresRepository {
	return &CashCloseEventPostgresRepository{}
}

func (r *CashCloseEventPostgresRepository) Create(tx sharedDomain.Transaction, e *cashCloseDomain.CashCloseEvent) (*cashCloseDomain.CashCloseEvent, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := cashCloseEventToModel(e)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return e, nil
}

func (r *CashCloseEventPostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*cashCloseDomain.CashCloseEvent, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var rows []models.CashCloseEventModel
	if err := gormTx.Where("gym_id = ? AND deleted_at IS NULL", gymID).
		Order("close_date DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*cashCloseDomain.CashCloseEvent, len(rows))
	for i := range rows {
		out[i] = cashCloseEventFromModel(&rows[i])
	}
	return out, nil
}

func cashCloseEventToModel(e *cashCloseDomain.CashCloseEvent) models.CashCloseEventModel {
	return models.CashCloseEventModel{
		ID:                e.ID,
		GymID:             e.GymID,
		Version:           e.Version,
		CreatedAt:         e.CreatedAt,
		UpdatedAt:         e.UpdatedAt,
		DeletedAt:         e.DeletedAt,
		CloseDate:         e.CloseDate,
		CalculatedCash:    e.CalculatedCash,
		CountedCash:       e.CountedCash,
		DiscrepancyReason: e.DiscrepancyReason,
		ClosedBy:          e.ClosedBy,
	}
}

func cashCloseEventFromModel(m *models.CashCloseEventModel) *cashCloseDomain.CashCloseEvent {
	return &cashCloseDomain.CashCloseEvent{
		ID:                m.ID,
		GymID:             m.GymID,
		Version:           m.Version,
		CloseDate:         time.Date(m.CloseDate.Year(), m.CloseDate.Month(), m.CloseDate.Day(), 0, 0, 0, 0, time.UTC),
		CalculatedCash:    m.CalculatedCash,
		CountedCash:       m.CountedCash,
		DiscrepancyReason: m.DiscrepancyReason,
		ClosedBy:          m.ClosedBy,
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
		DeletedAt:         m.DeletedAt,
	}
}
