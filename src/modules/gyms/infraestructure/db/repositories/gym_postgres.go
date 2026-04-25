//go:build server

package repositories

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	gymErrors "github.com/cuadra/cuadra-core/src/modules/gyms/domain/errors"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	"github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type GymPostgresRepository struct{}

func NewGymPostgresRepository() *GymPostgresRepository { return &GymPostgresRepository{} }

func (r *GymPostgresRepository) Create(tx sharedDomain.Transaction, g *gymDomain.Gym) (*gymDomain.Gym, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := toModel(g)
	if err := gormTx.Create(&m).Error; err != nil {
		return nil, err
	}
	return toDomain(&m), nil
}

func (r *GymPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*gymDomain.Gym, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var m models.GymModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(gymErrors.ErrGymNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return toDomain(&m), nil
}

func (r *GymPostgresRepository) Update(tx sharedDomain.Transaction, g *gymDomain.Gym) (*gymDomain.Gym, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	g.UpdatedAt = time.Now().UTC()
	m := toModel(g)
	// Omit id + created_at so we don't move them by accident.
	if err := gormTx.Where("id = ?", g.ID).
		Omit("id", "created_at", "gym_id").
		Save(&m).Error; err != nil {
		return nil, err
	}
	return toDomain(&m), nil
}

// HasMembershipType is the cross-BC peek the wizard needs to decide whether
// the user can complete setup (UC-001 step 5). We deliberately query the
// table directly instead of importing members/repository: this keeps the
// gyms BC self-contained and avoids a circular dependency at compile time.
func (r *GymPostgresRepository) HasMembershipType(tx sharedDomain.Transaction, gymID uuid.UUID) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var count int64
	if err := gormTx.Table("membership_types").
		Where("gym_id = ? AND deleted_at IS NULL", gymID).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}
