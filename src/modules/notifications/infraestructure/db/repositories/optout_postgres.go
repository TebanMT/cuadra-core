//go:build server

package repositories

import (
	"time"

	"gorm.io/gorm/clause"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// OptOutPostgresRepository implementa OptOutRepository sobre la tabla
// `whatsapp_opt_outs` (migración 031). Keyed por teléfono normalizado.
type OptOutPostgresRepository struct{}

func NewOptOutPostgresRepository() *OptOutPostgresRepository {
	return &OptOutPostgresRepository{}
}

type whatsappOptOutModel struct {
	Phone      string    `gorm:"primaryKey;column:phone"`
	OptedOutAt time.Time `gorm:"column:opted_out_at"`
}

func (whatsappOptOutModel) TableName() string { return "whatsapp_opt_outs" }

func (r *OptOutPostgresRepository) IsOptedOut(tx sharedDomain.Transaction, phone string) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var count int64
	if err := gormTx.Model(&whatsappOptOutModel{}).Where("phone = ?", phone).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *OptOutPostgresRepository) SetOptedOut(tx sharedDomain.Transaction, phone string, at time.Time) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	// Idempotente: si ya está en opt-out, no-op (conserva el opted_out_at viejo).
	return gormTx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "phone"}},
		DoNothing: true,
	}).Create(&whatsappOptOutModel{Phone: phone, OptedOutAt: at}).Error
}

func (r *OptOutPostgresRepository) ClearOptOut(tx sharedDomain.Transaction, phone string) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	return gormTx.Where("phone = ?", phone).Delete(&whatsappOptOutModel{}).Error
}
