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
// into ByMethod / ByConcept — they're surfaced in RefundTotal + RefundCount so
// the operator sees the gross take separately. ByConcept now carries counts;
// ByOperator brings the user.full_name + sales count via subqueries.
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
		Cnt     int
	}
	var conceptRows []conceptRow
	if err := gormTx.Model(&models.PaymentModel{}).
		Select("concept, SUM(amount) AS total, COUNT(*) AS cnt").
		Where("gym_id = ? AND payment_date = ? AND concept <> ? AND deleted_at IS NULL",
			q.GymID, dateStr, paymentDomain.ConceptRefund).
		Group("concept").Scan(&conceptRows).Error; err != nil {
		return nil, err
	}

	// Refunds agrupados por método: alimentan RefundTotal y RefundByMethod
	// (para descontar del cajón los refunds en efectivo). amount es negativo.
	type refundRow struct {
		PaymentMethod string
		Total         float64
		Cnt           int
	}
	var refundRows []refundRow
	if err := gormTx.Model(&models.PaymentModel{}).
		Select("payment_method, COALESCE(SUM(amount), 0) AS total, COUNT(*) AS cnt").
		Where("gym_id = ? AND payment_date = ? AND concept = ? AND deleted_at IS NULL",
			q.GymID, dateStr, paymentDomain.ConceptRefund).
		Group("payment_method").Scan(&refundRows).Error; err != nil {
		return nil, err
	}

	// Operator rollup: JOIN users so the FE renders names, and use
	// SUM(CASE WHEN concept='product' THEN 1 ELSE 0 END) for sales count.
	// Excludes refunds — those are visualized separately in the report.
	type opRow struct {
		OperatorID   uuid.UUID
		OperatorName *string
		Total        float64
		PaymentsN    int
		SalesN       int
	}
	var opRows []opRow
	if err := gormTx.Raw(`
		SELECT p.operator_id,
		       u.full_name AS operator_name,
		       COALESCE(SUM(p.amount), 0) AS total,
		       COUNT(*) AS payments_n,
		       SUM(CASE WHEN p.concept = 'product' THEN 1 ELSE 0 END) AS sales_n
		FROM payments p
		LEFT JOIN users u ON u.id = p.operator_id AND u.deleted_at IS NULL
		WHERE p.gym_id = ? AND p.payment_date = ?
		  AND p.concept <> ? AND p.deleted_at IS NULL
		GROUP BY p.operator_id, u.full_name
		ORDER BY total DESC`,
		q.GymID, dateStr, paymentDomain.ConceptRefund).Scan(&opRows).Error; err != nil {
		return nil, err
	}

	out := &billingRepo.CashCloseTotals{
		ByMethod:       map[string]float64{},
		ByConcept:      map[string]billingRepo.ConceptTotal{},
		ByOperator:     make([]billingRepo.OperatorTotal, 0, len(opRows)),
		RefundByMethod: map[string]float64{},
	}
	for _, rf := range refundRows {
		out.RefundByMethod[rf.PaymentMethod] = rf.Total // negativo
		out.RefundTotal += rf.Total
		out.RefundCount += rf.Cnt
	}
	for _, m := range methodRows {
		out.ByMethod[m.PaymentMethod] = m.Total
		out.GrandTotal += m.Total
	}
	for _, c := range conceptRows {
		out.ByConcept[c.Concept] = billingRepo.ConceptTotal{Total: c.Total, Count: c.Cnt}
	}
	for _, o := range opRows {
		name := ""
		if o.OperatorName != nil {
			name = *o.OperatorName
		}
		out.ByOperator = append(out.ByOperator, billingRepo.OperatorTotal{
			OperatorID:   o.OperatorID,
			OperatorName: name,
			Total:        o.Total,
			PaymentsN:    o.PaymentsN,
			SalesN:       o.SalesN,
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
