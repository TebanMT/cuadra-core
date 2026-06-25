//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"log"
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
	return p, nil
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

func (r *PaymentPostgresRepository) ListByGymBetweenDates(tx sharedDomain.Transaction, q billingRepo.ListByGymQuery) ([]*paymentDomain.Payment, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	page, pageSize := normalizePage(q.Page, q.PageSize)
	base := gormTx.Model(&models.PaymentModel{}).
		Where("gym_id = ? AND deleted_at IS NULL", q.GymID).
		Where("payment_date >= ? AND payment_date <= ?", q.From.Format("2006-01-02"), q.To.Format("2006-01-02"))
	if q.ConceptFilter != "" {
		base = base.Where("concept = ?", q.ConceptFilter)
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

func (r *PaymentPostgresRepository) HasRefundFor(tx sharedDomain.Transaction, parentPaymentID uuid.UUID) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.PaymentModel{}).
		Where("parent_payment_id = ? AND concept = ? AND deleted_at IS NULL",
			parentPaymentID, paymentDomain.ConceptRefund).
		Count(&n).Error
	return n > 0, err
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
	m := models.PaymentModel{
		ID:              p.ID,
		GymID:           p.GymID,
		Version:         p.Version,
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
		DeletedAt:       p.DeletedAt,
		Folio:           p.Folio,
		MemberID:        p.MemberID,
		Amount:          p.Amount,
		PaymentMethod:   p.PaymentMethod,
		Concept:         p.Concept,
		ParentPaymentID: p.ParentPaymentID,
		DiscountAmount:  p.DiscountAmount,
		DiscountReason:  p.DiscountReason,
		BalancePending:  p.BalancePending,
		PaymentDate:     p.PaymentDate,
		Notes:           p.Notes,
		OperatorID:      p.OperatorID,
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
		ID:              r.ID,
		GymID:           r.GymID,
		Version:         r.Version,
		Folio:           r.Folio,
		MemberID:        r.MemberID,
		Amount:          r.Amount,
		PaymentMethod:   r.PaymentMethod,
		Concept:         r.Concept,
		ParentPaymentID: r.ParentPaymentID,
		DiscountAmount:  r.DiscountAmount,
		DiscountReason:  r.DiscountReason,
		BalancePending:  r.BalancePending,
		PaymentDate:     r.PaymentDate,
		Notes:           r.Notes,
		OperatorID:      r.OperatorID,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
		DeletedAt:       r.DeletedAt,
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
