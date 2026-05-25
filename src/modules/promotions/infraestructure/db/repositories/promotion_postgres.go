//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	promoErrors "github.com/cuadra/cuadra-core/src/modules/promotions/domain/errors"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/promotions/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// PromotionPostgresRepository is the cloud-side Promotion repo.
type PromotionPostgresRepository struct{}

func NewPromotionPostgresRepository() *PromotionPostgresRepository {
	return &PromotionPostgresRepository{}
}

func (r *PromotionPostgresRepository) Create(tx sharedDomain.Transaction, p *promoDomain.Promotion) (*promoDomain.Promotion, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := promoToModel(p)
	if err := gormTx.Create(&m).Error; err != nil {
		return nil, err
	}
	if err := emitPromotionToSync(gormTx, p); err != nil {
		return nil, err
	}
	return promoFromModel(&m), nil
}

func (r *PromotionPostgresRepository) Update(tx sharedDomain.Transaction, p *promoDomain.Promotion) (*promoDomain.Promotion, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	p.UpdatedAt = time.Now().UTC()
	m := promoToModel(p)
	if err := gormTx.Where("id = ?", p.ID).Omit("id", "created_at", "gym_id").Save(&m).Error; err != nil {
		return nil, err
	}
	if err := emitPromotionToSync(gormTx, p); err != nil {
		return nil, err
	}
	return promoFromModel(&m), nil
}

func (r *PromotionPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*promoDomain.Promotion, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var m models.PromotionModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return promoFromModel(&m), nil
}

func (r *PromotionPostgresRepository) GetByCode(tx sharedDomain.Transaction, gymID uuid.UUID, codeLower string) (*promoDomain.Promotion, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var m models.PromotionModel
	err := gormTx.Where("gym_id = ? AND LOWER(code) = ? AND deleted_at IS NULL", gymID, codeLower).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionCodeNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return promoFromModel(&m), nil
}

func (r *PromotionPostgresRepository) List(tx sharedDomain.Transaction, f promoRepo.ListFilter) ([]*promoDomain.Promotion, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	q := gormTx.Where("gym_id = ? AND deleted_at IS NULL", f.GymID)
	if !f.IncludeInactive {
		q = q.Where("active = ?", true)
	}
	if f.AppliesTo != "" {
		// applies_to=any matchea cualquier target; filtramos por
		// (applies_to = $target OR applies_to = 'any').
		q = q.Where("applies_to IN ?", []string{f.AppliesTo, promoDomain.AppliesToAny})
	}
	if f.CurrentlyValid != nil {
		d := f.CurrentlyValid.UTC()
		q = q.Where("(valid_from IS NULL OR valid_from <= ?)", d).
			Where("(valid_until IS NULL OR valid_until >= ?)", d)
	}
	var rows []models.PromotionModel
	if err := q.Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*promoDomain.Promotion, len(rows))
	for i := range rows {
		out[i] = promoFromModel(&rows[i])
	}
	return out, nil
}

func (r *PromotionPostgresRepository) ExistsByCode(tx sharedDomain.Transaction, gymID uuid.UUID, codeLower string, excludeID *uuid.UUID) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	q := gormTx.Model(&models.PromotionModel{}).
		Where("gym_id = ? AND LOWER(code) = ? AND deleted_at IS NULL", gymID, strings.ToLower(strings.TrimSpace(codeLower)))
	if excludeID != nil {
		q = q.Where("id <> ?", *excludeID)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

// emitPromotionToSync espeja `promotions` en sync_entities — mismo
// patrón que emitMembershipTypeToSync. Cloud → sidecar pull se sirve
// desde aquí. Cents en el destino SQLite, pesos en el cloud (igual que
// membership_types).
func emitPromotionToSync(g *gorm.DB, p *promoDomain.Promotion) error {
	toCents := func(pesos *float64) *int64 {
		if pesos == nil {
			return nil
		}
		c := int64((*pesos)*100 + 0.5)
		return &c
	}
	payloadValue := func() any {
		switch p.Kind {
		case promoDomain.KindFixedAmount:
			return toCents(p.Value)
		case promoDomain.KindPercent, promoDomain.KindExtraDays:
			if p.Value == nil {
				return nil
			}
			v := int64(*p.Value)
			return &v
		default:
			return nil
		}
	}()
	var validFrom, validUntil any
	if p.ValidFrom != nil {
		validFrom = p.ValidFrom.UTC().Format("2006-01-02")
	}
	if p.ValidUntil != nil {
		validUntil = p.ValidUntil.UTC().Format("2006-01-02")
	}
	payload, err := json.Marshal(map[string]any{
		"id":                  p.ID.String(),
		"gym_id":              p.GymID.String(),
		"version":             p.Version,
		"name":                p.Name,
		"description":         p.Description,
		"kind":                p.Kind,
		"value":               payloadValue,
		"buy_n":               p.BuyN,
		"companion_count":     p.CompanionCount,
		"applies_to":          p.AppliesTo,
		"code":                p.Code,
		"valid_from":          validFrom,
		"valid_until":         validUntil,
		"max_uses_total":      p.MaxUsesTotal,
		"max_uses_per_member": p.MaxUsesPerMember,
		"active":              p.Active,
		"created_at":          p.CreatedAt.UnixMilli(),
		"updated_at":          p.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	var deletedAt any
	if p.DeletedAt != nil {
		deletedAt = *p.DeletedAt
	}
	return g.Exec(`
		INSERT INTO sync_entities
			(gym_id, entity_type, entity_id, version, payload, server_updated_at, deleted_at)
		VALUES (?, 'promotions', ?, ?, ?::jsonb, NOW(), ?)
		ON CONFLICT (gym_id, entity_type, entity_id) DO UPDATE SET
			version = EXCLUDED.version,
			payload = EXCLUDED.payload,
			server_updated_at = NOW(),
			deleted_at = EXCLUDED.deleted_at`,
		p.GymID, p.ID, p.Version, string(payload), deletedAt,
	).Error
}

func promoToModel(p *promoDomain.Promotion) models.PromotionModel {
	return models.PromotionModel{
		ID:               p.ID,
		GymID:            p.GymID,
		Version:          p.Version,
		CreatedAt:        p.CreatedAt,
		UpdatedAt:        p.UpdatedAt,
		DeletedAt:        p.DeletedAt,
		Name:             p.Name,
		Description:      p.Description,
		Kind:             p.Kind,
		Value:            p.Value,
		BuyN:             p.BuyN,
		CompanionCount:   p.CompanionCount,
		AppliesTo:        p.AppliesTo,
		Code:             p.Code,
		ValidFrom:        p.ValidFrom,
		ValidUntil:       p.ValidUntil,
		MaxUsesTotal:     p.MaxUsesTotal,
		MaxUsesPerMember: p.MaxUsesPerMember,
		Active:           p.Active,
	}
}

func promoFromModel(m *models.PromotionModel) *promoDomain.Promotion {
	return &promoDomain.Promotion{
		ID:               m.ID,
		GymID:            m.GymID,
		Version:          m.Version,
		Name:             m.Name,
		Description:      m.Description,
		Kind:             m.Kind,
		Value:            m.Value,
		BuyN:             m.BuyN,
		CompanionCount:   m.CompanionCount,
		AppliesTo:        m.AppliesTo,
		Code:             m.Code,
		ValidFrom:        m.ValidFrom,
		ValidUntil:       m.ValidUntil,
		MaxUsesTotal:     m.MaxUsesTotal,
		MaxUsesPerMember: m.MaxUsesPerMember,
		Active:           m.Active,
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
		DeletedAt:        m.DeletedAt,
	}
}
