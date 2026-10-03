//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type PaymentPostgresRepository struct{}

func NewPaymentPostgresRepository() *PaymentPostgresRepository {
	return &PaymentPostgresRepository{}
}

func (r *PaymentPostgresRepository) Create(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := paymentToModel(p)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	if err := mirrorPayment(gormTx, p); err != nil {
		return nil, err
	}
	return paymentFromModel(&row), nil
}

// Update is intentionally narrow: append-only schema means the only fields a
// client may legitimately mutate after insert are `notes` and `balance_pending`
// (the latter is decremented on UC-019 settlement). Everything else is
// preserved by the explicit Select clause.
func (r *PaymentPostgresRepository) Update(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	p.UpdatedAt = time.Now().UTC()
	if err := gormTx.Model(&models.PaymentModel{}).Where("id = ?", p.ID).
		Updates(map[string]any{
			"version":         p.Version,
			"updated_at":      p.UpdatedAt,
			"notes":           p.Notes,
			"balance_pending": p.BalancePending,
		}).Error; err != nil {
		return nil, err
	}
	if err := mirrorPayment(gormTx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *PaymentPostgresRepository) UpdateForSaleCorrection(tx sharedDomain.Transaction, p *paymentDomain.Payment) (*paymentDomain.Payment, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	breakdownBytes, err := json.Marshal(p.Breakdown)
	if err != nil {
		return nil, err
	}
	res := gormTx.Model(&models.PaymentModel{}).
		Where("gym_id=? AND id=? AND deleted_at IS NULL", p.GymID, p.ID).
		Updates(map[string]any{
			"version": p.Version, "updated_at": p.UpdatedAt, "amount": p.Amount,
			"recognized_amount": p.RecognizedAmount, "balance_pending": p.BalancePending,
			"discount_amount": p.DiscountAmount, "deleted_at": p.DeletedAt,
			"breakdown": gorm.Expr("?::jsonb", string(breakdownBytes)),
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrPaymentNotFound, "")
	}
	if err := mirrorPayment(gormTx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *PaymentPostgresRepository) UpdateForAdministrativeCorrection(tx sharedDomain.Transaction, p *paymentDomain.Payment, expectedVersion int) (*paymentDomain.Payment, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	res := gormTx.Model(&models.PaymentModel{}).
		Where("gym_id=? AND id=? AND version=? AND deleted_at IS NULL", p.GymID, p.ID, expectedVersion).
		Updates(map[string]any{
			"version": p.Version, "updated_at": p.UpdatedAt, "amount": p.Amount,
			"recognized_amount": p.RecognizedAmount, "balance_pending": p.BalancePending,
			"cash_destination": p.EffectiveCashDestination(), "payment_method": p.PaymentMethod, "cash_drawer_id": p.CashDrawerID,
			"payment_date": p.PaymentDate, "deleted_at": p.DeletedAt,
		})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrPaymentVersionConflict, "")
	}
	if err := mirrorPayment(gormTx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *PaymentPostgresRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*paymentDomain.Payment, error) {
	var row models.PaymentModel
	err := tx.(*sharedDomain.GormTransaction).Tx.
		Where("gym_id=? AND idempotency_key=? AND deleted_at IS NULL", gymID, strings.TrimSpace(key)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return paymentFromModel(&row), nil
}

func (r *PaymentPostgresRepository) FinalizeIdempotency(tx sharedDomain.Transaction, p *paymentDomain.Payment) error {
	if len(p.IdempotencyResult) == 0 {
		return billingErrors.ErrIdempotencyKeyRequired
	}
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	res := gormTx.Model(&models.PaymentModel{}).
		Where("gym_id=? AND id=? AND idempotency_key=? AND idempotency_fingerprint=? AND deleted_at IS NULL",
			p.GymID, p.ID, p.IdempotencyKey, p.IdempotencyFingerprint).
		Updates(map[string]any{"version": p.Version, "updated_at": p.UpdatedAt,
			"idempotency_result": gorm.Expr("?::jsonb", string(p.IdempotencyResult))})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return billingErrors.ErrIdempotencyKeyConflict
	}
	return mirrorPayment(gormTx, p)
}

func (r *PaymentPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*paymentDomain.Payment, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.PaymentModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrPaymentNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return paymentFromModel(&row), nil
}

func (r *PaymentPostgresRepository) ListByMember(tx sharedDomain.Transaction, q billingRepo.ListByMemberQuery) ([]*paymentDomain.Payment, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	page, pageSize := normalizePage(q.Page, q.PageSize)
	base := gormTx.Model(&models.PaymentModel{}).
		Where("gym_id = ? AND member_id = ? AND deleted_at IS NULL", q.GymID, q.MemberID)
	if q.ConceptFilter != "" {
		base = base.Where("concept = ?", q.ConceptFilter)
	}
	if q.From != nil {
		base = base.Where("payment_date >= ?", q.From.Format("2006-01-02"))
	}
	if q.To != nil {
		base = base.Where("payment_date <= ?", q.To.Format("2006-01-02"))
	}
	var total int64
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.PaymentModel
	if err := base.Order("payment_date DESC, created_at DESC").
		Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*paymentDomain.Payment, len(rows))
	for i := range rows {
		out[i] = paymentFromModel(&rows[i])
	}
	return out, int(total), nil
}

// SumPendingByMember — agregado server-side (ver el porqué en la interfaz):
// el NUMERIC del cloud ya está en pesos, va directo.
func (r *PaymentPostgresRepository) SumPendingByMember(tx sharedDomain.Transaction, gymID, memberID uuid.UUID) (float64, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var total float64
	err := gormTx.Model(&models.PaymentModel{}).
		Where("gym_id = ? AND member_id = ? AND deleted_at IS NULL", gymID, memberID).
		Select("COALESCE(SUM(balance_pending), 0)").
		Scan(&total).Error
	return total, err
}

func (r *PaymentPostgresRepository) ListByGymBetweenDates(tx sharedDomain.Transaction, q billingRepo.ListByGymQuery) ([]*paymentDomain.Payment, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	page, pageSize := normalizePage(q.Page, q.PageSize)
	base := gormTx.Model(&models.PaymentModel{}).
		Where("gym_id = ? AND deleted_at IS NULL", q.GymID).
		Where("payment_date >= ? AND payment_date <= ?", q.From.Format("2006-01-02"), q.To.Format("2006-01-02"))
	if q.ConceptFilter != "" {
		base = base.Where("concept = ?", q.ConceptFilter)
	}
	if q.MethodFilter != "" {
		base = base.Where("payment_method = ?", q.MethodFilter)
	}
	var total int64
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.PaymentModel
	if err := base.Order("payment_date DESC, created_at DESC").
		Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]*paymentDomain.Payment, len(rows))
	for i := range rows {
		out[i] = paymentFromModel(&rows[i])
	}
	return out, int(total), nil
}

func (r *PaymentPostgresRepository) AggregateByGymBetweenDates(tx sharedDomain.Transaction, q billingRepo.ListByGymQuery) (billingRepo.ListByGymAggregates, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row struct {
		NetTotal      float64
		RefundTotal   float64
		CashTotal     float64
		TransferTotal float64
		CardTotal     float64
	}
	query := `
		SELECT
		  COALESCE(SUM(amount), 0) AS net_total,
		  COALESCE(SUM(ABS(amount)) FILTER (WHERE concept = 'refund'), 0) AS refund_total,
		  COALESCE(SUM(amount) FILTER (WHERE payment_method = 'cash'), 0) AS cash_total,
		  COALESCE(SUM(amount) FILTER (WHERE payment_method = 'transfer'), 0) AS transfer_total,
		  COALESCE(SUM(amount) FILTER (WHERE payment_method = 'card'), 0) AS card_total
		FROM payments
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND payment_date >= ? AND payment_date <= ?`
	args := []any{q.GymID, q.From.Format("2006-01-02"), q.To.Format("2006-01-02")}
	if q.ConceptFilter != "" {
		query += ` AND concept = ?`
		args = append(args, q.ConceptFilter)
	}
	if q.MethodFilter != "" {
		query += ` AND payment_method = ?`
		args = append(args, q.MethodFilter)
	}
	if err := gormTx.Raw(query, args...).Scan(&row).Error; err != nil {
		return billingRepo.ListByGymAggregates{}, err
	}
	return billingRepo.ListByGymAggregates{
		NetTotal:      row.NetTotal,
		RefundTotal:   row.RefundTotal,
		CashTotal:     row.CashTotal,
		TransferTotal: row.TransferTotal,
		CardTotal:     row.CardTotal,
	}, nil
}

func (r *PaymentPostgresRepository) HasRefundFor(tx sharedDomain.Transaction, parentPaymentID uuid.UUID) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.PaymentModel{}).
		Where("parent_payment_id = ? AND concept = ? AND deleted_at IS NULL",
			parentPaymentID, paymentDomain.ConceptRefund).
		Count(&n).Error
	return n > 0, err
}

func (r *PaymentPostgresRepository) RefundBalanceForUpdate(tx sharedDomain.Transaction, gymID, rootPaymentID uuid.UUID) (billingRepo.RefundBalance, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	return r.refundBalance(gormTx, gymID, rootPaymentID, true)
}

func (r *PaymentPostgresRepository) RefundBalance(tx sharedDomain.Transaction, gymID, rootPaymentID uuid.UUID) (billingRepo.RefundBalance, error) {
	return r.refundBalance(tx.(*sharedDomain.GormTransaction).Tx, gymID, rootPaymentID, false)
}

func (r *PaymentPostgresRepository) refundBalance(gormTx *gorm.DB, gymID, rootPaymentID uuid.UUID, lock bool) (billingRepo.RefundBalance, error) {
	var root models.PaymentModel
	query := gormTx
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := query.Where("gym_id=? AND id=? AND deleted_at IS NULL", gymID, rootPaymentID).First(&root).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return billingRepo.RefundBalance{}, sharedDomain.NewBusinessError(billingErrors.ErrPaymentNotFound, "")
	}
	if err != nil {
		return billingRepo.RefundBalance{}, err
	}
	var sums struct{ Collected, Recognized, Refunded, SourceRefunded float64 }
	if err := gormTx.Raw(`
		SELECT ? + COALESCE(SUM(amount) FILTER (WHERE concept='balance_settlement'),0) AS collected,
		       ? + COALESCE(SUM(recognized_amount) FILTER (WHERE concept='balance_settlement'),0) AS recognized,
		       COALESCE(SUM(ABS(amount)) FILTER (WHERE concept='refund'),0) AS refunded,
		       COALESCE(SUM(ABS(amount)) FILTER (
		         WHERE concept='refund' AND parent_payment_id=?),0) AS source_refunded
		FROM payments p
		WHERE p.gym_id=? AND p.deleted_at IS NULL AND (
		  p.parent_payment_id=? OR (
		    p.concept='refund' AND EXISTS(
		      SELECT 1 FROM payments source
		      WHERE source.id=p.parent_payment_id AND source.gym_id=p.gym_id
		        AND source.concept='balance_settlement'
		        AND source.parent_payment_id=? AND source.deleted_at IS NULL
		    )
		  )
		)`,
		root.Amount, root.RecognizedAmount, rootPaymentID, gymID, rootPaymentID, rootPaymentID).Scan(&sums).Error; err != nil {
		return billingRepo.RefundBalance{}, err
	}
	refundable := sums.Collected - sums.Refunded
	if refundable < 0 {
		refundable = 0
	}
	sourceRefundable := root.Amount - sums.SourceRefunded
	if sourceRefundable < 0 {
		sourceRefundable = 0
	}
	return billingRepo.RefundBalance{
		Root: paymentFromModel(&root), Collected: sums.Collected, Recognized: sums.Recognized,
		Refunded: sums.Refunded, Refundable: refundable,
		SourceRefunded: sums.SourceRefunded, SourceRefundable: sourceRefundable,
	}, nil
}

// MaxFolioForConcept locks the matching gym/concept folio range with FOR
// UPDATE so concurrent INSERTs queue behind us.
func (r *PaymentPostgresRepository) MaxFolioForConcept(tx sharedDomain.Transaction, gymID uuid.UUID, concept string) (string, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var max struct {
		Max *string
	}
	// Ordena por el valor NUMÉRICO del folio (lo de después del '-'), no
	// lexicográfico: MAX(folio) como texto daba "MEM-99999" > "MEM-100000"
	// (porque '9' > '1'), y el siguiente folio calculado colisionaba con uno
	// ya existente (p.ej. importado con MEM-%05d de IDs legacy > 99999). Con
	// FOR UPDATE sobre la fila ganadora se serializa la asignación de folios.
	err := gormTx.Model(&models.PaymentModel{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("folio AS max").
		Where("gym_id = ? AND concept = ?", gymID, concept).
		Order("NULLIF(split_part(folio, '-', 2), '')::int DESC NULLS LAST, folio DESC").
		Limit(1).
		Scan(&max).Error
	if err != nil {
		return "", err
	}
	if max.Max == nil {
		return "", nil
	}
	return *max.Max, nil
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func paymentToModel(p *paymentDomain.Payment) models.PaymentModel {
	var cashDrawerID *uuid.UUID
	if p.PaymentMethod == paymentDomain.MethodCash && p.EffectiveCashDestination() != "gym_fund" {
		drawerID := p.GymID
		if p.CashDrawerID != nil && *p.CashDrawerID != uuid.Nil {
			drawerID = *p.CashDrawerID
		}
		cashDrawerID = &drawerID
	}
	m := models.PaymentModel{
		ID:               p.ID,
		GymID:            p.GymID,
		Version:          p.Version,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
		DeletedAt:        p.DeletedAt,
		Folio:            p.Folio,
		MemberID:         p.MemberID,
		MembershipID:     p.MembershipID,
		Amount:           p.Amount,
		RecognizedAmount: p.RecognizedAmount,
		CashDestination:  p.EffectiveCashDestination(),
		PaymentMethod:    p.PaymentMethod,
		CashDrawerID:     cashDrawerID,
		Concept:          p.Concept,
		ParentPaymentID:  p.ParentPaymentID,
		DiscountAmount:   p.DiscountAmount,
		DiscountReason:   p.DiscountReason,
		BalancePending:   p.BalancePending,
		PaymentDate:      p.PaymentDate,
		Notes:            p.Notes,
		OperatorID:       p.OperatorID,
	}
	if p.IdempotencyKey != "" {
		m.IdempotencyKey = &p.IdempotencyKey
		m.IdempotencyFingerprint = &p.IdempotencyFingerprint
	}
	if len(p.IdempotencyResult) > 0 {
		result := string(p.IdempotencyResult)
		m.IdempotencyResult = &result
	}
	if len(p.Breakdown) > 0 {
		if b, err := json.Marshal(p.Breakdown); err == nil {
			s := string(b)
			m.Breakdown = &s
		} else {
			log.Printf("[payment] breakdown marshal failed (payment=%s): %v", p.ID, err)
		}
	}
	return m
}

func paymentFromModel(r *models.PaymentModel) *paymentDomain.Payment {
	p := &paymentDomain.Payment{
		ID:               r.ID,
		GymID:            r.GymID,
		Version:          r.Version,
		Folio:            r.Folio,
		MemberID:         r.MemberID,
		MembershipID:     r.MembershipID,
		Amount:           r.Amount,
		RecognizedAmount: r.RecognizedAmount,
		CashDestination:  r.CashDestination,
		PaymentMethod:    r.PaymentMethod,
		CashDrawerID:     r.CashDrawerID,
		Concept:          r.Concept,
		ParentPaymentID:  r.ParentPaymentID,
		DiscountAmount:   r.DiscountAmount,
		DiscountReason:   r.DiscountReason,
		BalancePending:   r.BalancePending,
		PaymentDate:      r.PaymentDate,
		Notes:            r.Notes,
		OperatorID:       r.OperatorID,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
		DeletedAt:        r.DeletedAt,
	}
	if r.IdempotencyKey != nil {
		p.IdempotencyKey = *r.IdempotencyKey
	}
	if r.IdempotencyFingerprint != nil {
		p.IdempotencyFingerprint = *r.IdempotencyFingerprint
	}
	if r.IdempotencyResult != nil {
		p.IdempotencyResult = append([]byte(nil), (*r.IdempotencyResult)...)
	}
	if r.Breakdown != nil && *r.Breakdown != "" {
		var lines []paymentDomain.BreakdownLine
		if err := json.Unmarshal([]byte(*r.Breakdown), &lines); err == nil {
			p.Breakdown = lines
		} else {
			log.Printf("[payment] breakdown unmarshal failed (payment=%s): %v", r.ID, err)
		}
	}
	return p
}

func mirrorPayment(g *gorm.DB, p *paymentDomain.Payment) error {
	payload, err := json.Marshal(map[string]any{
		"id": p.ID, "gym_id": p.GymID, "version": p.Version,
		"created_at": p.CreatedAt.UnixMilli(), "updated_at": p.UpdatedAt.UnixMilli(),
		"deleted_at": p.DeletedAt, "folio": p.Folio, "member_id": p.MemberID, "membership_id": p.MembershipID,
		"idempotency_key":         nullablePaymentString(p.IdempotencyKey),
		"idempotency_fingerprint": nullablePaymentString(p.IdempotencyFingerprint),
		"idempotency_result":      json.RawMessage(p.IdempotencyResult),
		"amount":                  p.Amount, "recognized_amount": p.RecognizedAmount,
		"cash_destination": p.EffectiveCashDestination(), "payment_method": p.PaymentMethod, "cash_drawer_id": p.CashDrawerID,
		"concept": p.Concept, "parent_payment_id": p.ParentPaymentID,
		"discount_amount": p.DiscountAmount, "discount_reason": p.DiscountReason,
		"balance_pending": p.BalancePending, "payment_date": p.PaymentDate.Format("2006-01-02"),
		"notes": p.Notes, "breakdown": p.Breakdown, "operator_id": p.OperatorID,
	})
	if err != nil {
		return err
	}
	return upsertFinancialMirror(g, p.GymID, "payments", p.ID, p.Version, payload, p.DeletedAt)
}

func nullablePaymentString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
