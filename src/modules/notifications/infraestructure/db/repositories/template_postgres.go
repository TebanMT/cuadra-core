//go:build server

package repositories

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	tplDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/template"
	"github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type TemplateOverridePostgresRepository struct{}

func NewTemplateOverridePostgresRepository() *TemplateOverridePostgresRepository {
	return &TemplateOverridePostgresRepository{}
}

// Upsert creates or updates the row matching (gym_id, template_key). The
// caller's `o` already passed validation; we just persist.
func (r *TemplateOverridePostgresRepository) Upsert(tx sharedDomain.Transaction, o *tplDomain.Override) (*tplDomain.Override, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := overrideToModel(o)
	var existing models.TemplateOverrideModel
	err := gormTx.Where("gym_id = ? AND template_key = ? AND deleted_at IS NULL", o.GymID, o.TemplateKey).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if err := gormTx.Create(&row).Error; err != nil {
			return nil, err
		}
		return overrideFromModel(&row), nil
	}
	if err != nil {
		return nil, err
	}
	row.ID = existing.ID
	row.CreatedAt = existing.CreatedAt
	row.UpdatedAt = time.Now().UTC()
	if err := gormTx.Model(&models.TemplateOverrideModel{}).Where("id = ?", existing.ID).
		Updates(map[string]any{
			"version":    row.Version,
			"updated_at": row.UpdatedAt,
			"body":       row.Body,
			"enabled":    row.Enabled,
		}).Error; err != nil {
		return nil, err
	}
	o.ID = row.ID
	o.CreatedAt = row.CreatedAt
	o.UpdatedAt = row.UpdatedAt
	return o, nil
}

func (r *TemplateOverridePostgresRepository) GetByGymAndKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*tplDomain.Override, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.TemplateOverrideModel
	err := gormTx.Where("gym_id = ? AND template_key = ? AND deleted_at IS NULL", gymID, key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return overrideFromModel(&row), nil
}

func (r *TemplateOverridePostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*tplDomain.Override, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.TemplateOverrideModel
	if err := gormTx.Where("gym_id = ? AND deleted_at IS NULL", gymID).Order("template_key ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*tplDomain.Override, len(rows))
	for i := range rows {
		out[i] = overrideFromModel(&rows[i])
	}
	return out, nil
}

func overrideToModel(o *tplDomain.Override) models.TemplateOverrideModel {
	return models.TemplateOverrideModel{
		ID:          o.ID,
		GymID:       o.GymID,
		Version:     o.Version,
		CreatedAt:   o.CreatedAt,
		UpdatedAt:   o.UpdatedAt,
		DeletedAt:   o.DeletedAt,
		TemplateKey: o.TemplateKey,
		Body:        o.Body,
		Enabled:     o.Enabled,
	}
}

func overrideFromModel(m *models.TemplateOverrideModel) *tplDomain.Override {
	return &tplDomain.Override{
		ID:          m.ID,
		GymID:       m.GymID,
		Version:     m.Version,
		TemplateKey: m.TemplateKey,
		Body:        m.Body,
		Enabled:     m.Enabled,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
		DeletedAt:   m.DeletedAt,
	}
}

// guard against unused imports for the leaner builds.
var _ = notiErrors.ErrTemplateNotFound
