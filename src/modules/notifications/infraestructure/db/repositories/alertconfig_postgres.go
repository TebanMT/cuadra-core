//go:build server

package repositories

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	alertDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/alertconfig"
	"github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type AlertConfigPostgresRepository struct{}

func NewAlertConfigPostgresRepository() *AlertConfigPostgresRepository {
	return &AlertConfigPostgresRepository{}
}

// Upsert writes the (gym_id, alert_key) row. The composite primary key drives
// the conflict resolution — no surrogate id like the other tables.
func (r *AlertConfigPostgresRepository) Upsert(tx sharedDomain.Transaction, c *alertDomain.Config) (*alertDomain.Config, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := alertConfigToModel(c)
	var existing models.AlertConfigModel
	err := gormTx.Where("gym_id = ? AND alert_key = ? AND deleted_at IS NULL", c.GymID, string(c.Key)).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if err := gormTx.Create(&row).Error; err != nil {
			return nil, err
		}
		return alertConfigFromModel(&row), nil
	}
	if err != nil {
		return nil, err
	}
	row.UpdatedAt = time.Now().UTC()
	if err := gormTx.Model(&models.AlertConfigModel{}).
		Where("gym_id = ? AND alert_key = ?", c.GymID, string(c.Key)).
		Updates(map[string]any{
			"enabled":    row.Enabled,
			"version":    row.Version,
			"updated_at": row.UpdatedAt,
		}).Error; err != nil {
		return nil, err
	}
	c.UpdatedAt = row.UpdatedAt
	return c, nil
}

func (r *AlertConfigPostgresRepository) GetByGymAndKey(tx sharedDomain.Transaction, gymID uuid.UUID, key alertDomain.Key) (*alertDomain.Config, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.AlertConfigModel
	err := gormTx.Where("gym_id = ? AND alert_key = ? AND deleted_at IS NULL", gymID, string(key)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return alertConfigFromModel(&row), nil
}

func (r *AlertConfigPostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*alertDomain.Config, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.AlertConfigModel
	if err := gormTx.Where("gym_id = ? AND deleted_at IS NULL", gymID).Order("alert_key ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*alertDomain.Config, len(rows))
	for i := range rows {
		out[i] = alertConfigFromModel(&rows[i])
	}
	return out, nil
}

func alertConfigToModel(c *alertDomain.Config) models.AlertConfigModel {
	return models.AlertConfigModel{
		GymID:     c.GymID,
		AlertKey:  string(c.Key),
		Enabled:   c.Enabled,
		Version:   c.Version,
		UpdatedAt: c.UpdatedAt,
		DeletedAt: c.DeletedAt,
	}
}

func alertConfigFromModel(m *models.AlertConfigModel) *alertDomain.Config {
	return &alertDomain.Config{
		GymID:     m.GymID,
		Key:       alertDomain.Key(m.AlertKey),
		Enabled:   m.Enabled,
		Version:   m.Version,
		UpdatedAt: m.UpdatedAt,
		DeletedAt: m.DeletedAt,
	}
}
