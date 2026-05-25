//go:build server

package repositories

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/promotions/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// AppliedPromotionPostgresRepository cloud-side log de aplicaciones.
type AppliedPromotionPostgresRepository struct{}

func NewAppliedPromotionPostgresRepository() *AppliedPromotionPostgresRepository {
	return &AppliedPromotionPostgresRepository{}
}

func (r *AppliedPromotionPostgresRepository) Create(tx sharedDomain.Transaction, ap *promoDomain.AppliedPromotion) (*promoDomain.AppliedPromotion, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := appliedToModel(ap)
	if err := gormTx.Create(&m).Error; err != nil {
		return nil, err
	}
	if err := emitAppliedToSync(gormTx, ap); err != nil {
		return nil, err
	}
	return appliedFromModel(&m), nil
}

func (r *AppliedPromotionPostgresRepository) CountByPromotion(tx sharedDomain.Transaction, promotionID uuid.UUID) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.AppliedPromotionModel{}).
		Where("promotion_id = ? AND deleted_at IS NULL", promotionID).Count(&n).Error
	return int(n), err
}

func (r *AppliedPromotionPostgresRepository) CountByPromotionAndMember(tx sharedDomain.Transaction, promotionID, memberID uuid.UUID) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.AppliedPromotionModel{}).
		Where("promotion_id = ? AND member_id = ? AND deleted_at IS NULL", promotionID, memberID).Count(&n).Error
	return int(n), err
}

// SummaryByMonth — agrega aplicaciones del mes por promotion_id. El
// MembersReached cuenta DISTINCT member_id no-null (las ventas a
// walk-in no aportan al conteo).
type summaryRow struct {
	PromotionID    uuid.UUID `gorm:"column:promotion_id"`
	PromotionName  string    `gorm:"column:promotion_name_snapshot"`
	Kind           string    `gorm:"column:kind_snapshot"`
	UseCount       int       `gorm:"column:use_count"`
	TotalDiscount  float64   `gorm:"column:total_discount"`
	MembersReached int       `gorm:"column:members_reached"`
}

func (r *AppliedPromotionPostgresRepository) SummaryByMonth(tx sharedDomain.Transaction, gymID uuid.UUID, monthStart, monthEnd time.Time) ([]promoRepo.AppliedSummary, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []summaryRow
	// MAX(promotion_name_snapshot)/MAX(kind_snapshot) son legales porque
	// agrupamos por promotion_id; el snapshot de la última aplicación es
	// el que mostramos (más fresco). Si una promo cambió de nombre en el
	// mes, el rollup muestra el último nombre.
	err := gormTx.Raw(`
		SELECT
			promotion_id,
			MAX(promotion_name_snapshot) AS promotion_name_snapshot,
			MAX(kind_snapshot) AS kind_snapshot,
			COUNT(*) AS use_count,
			COALESCE(SUM(discount_amount), 0) AS total_discount,
			COUNT(DISTINCT member_id) FILTER (WHERE member_id IS NOT NULL) AS members_reached
		FROM applied_promotions
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND created_at >= ? AND created_at < ?
		GROUP BY promotion_id
		ORDER BY total_discount DESC, use_count DESC`,
		gymID, monthStart.UTC(), monthEnd.UTC(),
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]promoRepo.AppliedSummary, len(rows))
	for i, r := range rows {
		out[i] = promoRepo.AppliedSummary{
			PromotionID:    r.PromotionID,
			PromotionName:  r.PromotionName,
			Kind:           r.Kind,
			UseCount:       r.UseCount,
			TotalDiscount:  r.TotalDiscount,
			MembersReached: r.MembersReached,
		}
	}
	return out, nil
}

func emitAppliedToSync(g *gorm.DB, ap *promoDomain.AppliedPromotion) error {
	toCents := func(pesos *float64) *int64 {
		if pesos == nil {
			return nil
		}
		c := int64((*pesos)*100 + 0.5)
		return &c
	}
	var valueSnap any
	// El value_snapshot guarda lo mismo que el value del kind original. Para
	// fixed_amount es pesos; SQLite lo guarda en cents. Para percent y
	// extra_days es un entero pequeño.
	switch ap.KindSnapshot {
	case promoDomain.KindFixedAmount:
		valueSnap = toCents(ap.ValueSnapshot)
	case promoDomain.KindPercent, promoDomain.KindExtraDays:
		if ap.ValueSnapshot != nil {
			v := int64(*ap.ValueSnapshot)
			valueSnap = &v
		}
	}
	var memberID any
	if ap.MemberID != nil {
		memberID = ap.MemberID.String()
	}
	payload, err := json.Marshal(map[string]any{
		"id":                      ap.ID.String(),
		"gym_id":                  ap.GymID.String(),
		"version":                 ap.Version,
		"promotion_id":            ap.PromotionID.String(),
		"payment_id":              ap.PaymentID.String(),
		"member_id":               memberID,
		"applied_by_user_id":      ap.AppliedByUserID.String(),
		"promotion_name_snapshot": ap.PromotionNameSnapshot,
		"kind_snapshot":           ap.KindSnapshot,
		"value_snapshot":          valueSnap,
		"discount_amount":         int64(ap.DiscountAmount*100 + 0.5),
		"extra_days_applied":      ap.ExtraDaysApplied,
		"notes":                   ap.Notes,
		"created_at":              ap.CreatedAt.UnixMilli(),
		"updated_at":              ap.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	var deletedAt any
	if ap.DeletedAt != nil {
		deletedAt = *ap.DeletedAt
	}
	return g.Exec(`
		INSERT INTO sync_entities
			(gym_id, entity_type, entity_id, version, payload, server_updated_at, deleted_at)
		VALUES (?, 'applied_promotions', ?, ?, ?::jsonb, NOW(), ?)
		ON CONFLICT (gym_id, entity_type, entity_id) DO UPDATE SET
			version = EXCLUDED.version,
			payload = EXCLUDED.payload,
			server_updated_at = NOW(),
			deleted_at = EXCLUDED.deleted_at`,
		ap.GymID, ap.ID, ap.Version, string(payload), deletedAt,
	).Error
}

func appliedToModel(ap *promoDomain.AppliedPromotion) models.AppliedPromotionModel {
	return models.AppliedPromotionModel{
		ID:                    ap.ID,
		GymID:                 ap.GymID,
		Version:               ap.Version,
		CreatedAt:             ap.CreatedAt,
		UpdatedAt:             ap.UpdatedAt,
		DeletedAt:             ap.DeletedAt,
		PromotionID:           ap.PromotionID,
		PaymentID:             ap.PaymentID,
		MemberID:              ap.MemberID,
		AppliedByUserID:       ap.AppliedByUserID,
		PromotionNameSnapshot: ap.PromotionNameSnapshot,
		KindSnapshot:          ap.KindSnapshot,
		ValueSnapshot:         ap.ValueSnapshot,
		DiscountAmount:        ap.DiscountAmount,
		ExtraDaysApplied:      ap.ExtraDaysApplied,
		Notes:                 ap.Notes,
	}
}

func appliedFromModel(m *models.AppliedPromotionModel) *promoDomain.AppliedPromotion {
	return &promoDomain.AppliedPromotion{
		ID:                    m.ID,
		GymID:                 m.GymID,
		Version:               m.Version,
		PromotionID:           m.PromotionID,
		PaymentID:             m.PaymentID,
		MemberID:              m.MemberID,
		AppliedByUserID:       m.AppliedByUserID,
		PromotionNameSnapshot: m.PromotionNameSnapshot,
		KindSnapshot:          m.KindSnapshot,
		ValueSnapshot:         m.ValueSnapshot,
		DiscountAmount:        m.DiscountAmount,
		ExtraDaysApplied:      m.ExtraDaysApplied,
		Notes:                 m.Notes,
		CreatedAt:             m.CreatedAt,
		UpdatedAt:             m.UpdatedAt,
		DeletedAt:             m.DeletedAt,
	}
}
