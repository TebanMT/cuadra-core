//go:build server

package repositories

import (
	"strings"

	"github.com/google/uuid"

	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	"github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type MembershipTypePostgresRepository struct{}

func NewMembershipTypePostgresRepository() *MembershipTypePostgresRepository {
	return &MembershipTypePostgresRepository{}
}

func (r *MembershipTypePostgresRepository) Create(tx sharedDomain.Transaction, mt *mtDomain.MembershipType) (*mtDomain.MembershipType, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := mtToModel(mt)
	if err := gormTx.Create(&m).Error; err != nil {
		return nil, err
	}
	return mtFromModel(&m), nil
}

func (r *MembershipTypePostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*mtDomain.MembershipType, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.MembershipTypeModel
	if err := gormTx.Where("gym_id = ? AND deleted_at IS NULL", gymID).
		Order("created_at ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*mtDomain.MembershipType, len(rows))
	for i := range rows {
		out[i] = mtFromModel(&rows[i])
	}
	return out, nil
}

func (r *MembershipTypePostgresRepository) ExistsByGymAndName(tx sharedDomain.Transaction, gymID uuid.UUID, name string) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.MembershipTypeModel{}).
		Where("gym_id = ? AND LOWER(name) = ? AND deleted_at IS NULL", gymID, strings.ToLower(name)).
		Count(&n).Error
	return n > 0, err
}

func mtToModel(mt *mtDomain.MembershipType) models.MembershipTypeModel {
	return models.MembershipTypeModel{
		ID:                   mt.ID,
		GymID:                mt.GymID,
		Version:              mt.Version,
		CreatedAt:            mt.CreatedAt,
		UpdatedAt:            mt.UpdatedAt,
		DeletedAt:            mt.DeletedAt,
		Name:                 mt.Name,
		Price:                mt.Price,
		DurationDays:         mt.DurationDays,
		EnrollmentFee:        mt.EnrollmentFee,
		MaintenanceFee:       mt.MaintenanceFee,
		MaintenanceFrequency: mt.MaintenanceFrequency,
		Active:               mt.Active,
	}
}

func mtFromModel(m *models.MembershipTypeModel) *mtDomain.MembershipType {
	return &mtDomain.MembershipType{
		ID:                   m.ID,
		GymID:                m.GymID,
		Version:              m.Version,
		Name:                 m.Name,
		Price:                m.Price,
		DurationDays:         m.DurationDays,
		EnrollmentFee:        m.EnrollmentFee,
		MaintenanceFee:       m.MaintenanceFee,
		MaintenanceFrequency: m.MaintenanceFrequency,
		Active:               m.Active,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
		DeletedAt:            m.DeletedAt,
	}
}
