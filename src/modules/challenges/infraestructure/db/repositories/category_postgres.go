//go:build server

package repositories

import (
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	categoryDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/category"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/challenges/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CategoryPostgresRepository struct{}

func NewCategoryPostgresRepository() *CategoryPostgresRepository {
	return &CategoryPostgresRepository{}
}

func (r *CategoryPostgresRepository) Create(tx sharedDomain.Transaction, c *categoryDomain.Category) (*categoryDomain.Category, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := categoryToModel(c)
	if err := gormTx.Create(&m).Error; err != nil {
		return nil, err
	}
	return categoryToDomain(&m), nil
}

func (r *CategoryPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*categoryDomain.Category, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var m models.CategoryModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrCategoryNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return categoryToDomain(&m), nil
}

func (r *CategoryPostgresRepository) Update(tx sharedDomain.Transaction, c *categoryDomain.Category) (*categoryDomain.Category, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := categoryToModel(c)
	if err := gormTx.Where("id = ?", c.ID).Save(&m).Error; err != nil {
		return nil, err
	}
	return categoryToDomain(&m), nil
}

func (r *CategoryPostgresRepository) SoftDelete(tx sharedDomain.Transaction, id uuid.UUID) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	return gormTx.Exec(
		`UPDATE challenge_categories SET deleted_at = NOW(), updated_at = NOW(), version = version + 1
		 WHERE id = ? AND deleted_at IS NULL`, id).Error
}

func (r *CategoryPostgresRepository) ListByChallenge(tx sharedDomain.Transaction, challengeID uuid.UUID) ([]*categoryDomain.Category, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.CategoryModel
	if err := gormTx.
		Where("challenge_id = ? AND deleted_at IS NULL", challengeID).
		Order("sort_order ASC, created_at ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*categoryDomain.Category, len(rows))
	for i := range rows {
		out[i] = categoryToDomain(&rows[i])
	}
	return out, nil
}

func (r *CategoryPostgresRepository) CountParticipants(tx sharedDomain.Transaction, categoryID uuid.UUID) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.ParticipantModel{}).
		Where("category_id = ? AND deleted_at IS NULL", categoryID).
		Count(&n).Error
	return int(n), err
}

// ─── mappers ───────────────────────────────────────────────────────────────

func categoryToModel(c *categoryDomain.Category) models.CategoryModel {
	return models.CategoryModel{
		ID:          c.ID,
		GymID:       c.GymID,
		ChallengeID: c.ChallengeID,
		Version:     c.Version,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
		DeletedAt:   c.DeletedAt,
		Name:        c.Name,
		SortOrder:   c.SortOrder,
	}
}

func categoryToDomain(m *models.CategoryModel) *categoryDomain.Category {
	return &categoryDomain.Category{
		ID:          m.ID,
		GymID:       m.GymID,
		ChallengeID: m.ChallengeID,
		Version:     m.Version,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
		DeletedAt:   m.DeletedAt,
		Name:        m.Name,
		SortOrder:   m.SortOrder,
	}
}
