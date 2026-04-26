//go:build server

package repositories

import (
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	caDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/contact_attempt"
	"github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ContactAttemptPostgresRepository struct{}

func NewContactAttemptPostgresRepository() *ContactAttemptPostgresRepository {
	return &ContactAttemptPostgresRepository{}
}

func (r *ContactAttemptPostgresRepository) Create(tx sharedDomain.Transaction, a *caDomain.ContactAttempt) (*caDomain.ContactAttempt, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := contactAttemptToModel(a)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return contactAttemptFromModel(&row), nil
}

func (r *ContactAttemptPostgresRepository) LastByMember(tx sharedDomain.Transaction, memberID uuid.UUID) (*caDomain.ContactAttempt, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.ContactAttemptModel
	err := gormTx.Where("member_id = ? AND deleted_at IS NULL", memberID).
		Order("attempt_at DESC").Limit(1).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return contactAttemptFromModel(&row), nil
}

func (r *ContactAttemptPostgresRepository) ListByMember(tx sharedDomain.Transaction, memberID uuid.UUID, limit int) ([]*caDomain.ContactAttempt, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	var rows []models.ContactAttemptModel
	if err := gormTx.Where("member_id = ? AND deleted_at IS NULL", memberID).
		Order("attempt_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*caDomain.ContactAttempt, len(rows))
	for i := range rows {
		out[i] = contactAttemptFromModel(&rows[i])
	}
	return out, nil
}

func contactAttemptToModel(a *caDomain.ContactAttempt) models.ContactAttemptModel {
	return models.ContactAttemptModel{
		ID:          a.ID,
		GymID:       a.GymID,
		Version:     a.Version,
		CreatedAt:   a.CreatedAt,
		UpdatedAt:   a.UpdatedAt,
		DeletedAt:   a.DeletedAt,
		MemberID:    a.MemberID,
		AttemptAt:   a.AttemptAt,
		Channel:     a.Channel,
		Note:        a.Note,
		ContactedBy: a.ContactedBy,
	}
}

func contactAttemptFromModel(m *models.ContactAttemptModel) *caDomain.ContactAttempt {
	return &caDomain.ContactAttempt{
		ID:          m.ID,
		GymID:       m.GymID,
		Version:     m.Version,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
		DeletedAt:   m.DeletedAt,
		MemberID:    m.MemberID,
		AttemptAt:   m.AttemptAt,
		Channel:     m.Channel,
		Note:        m.Note,
		ContactedBy: m.ContactedBy,
	}
}
