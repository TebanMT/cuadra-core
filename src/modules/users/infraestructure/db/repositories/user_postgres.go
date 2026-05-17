//go:build server

package repositories

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	"github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type UserPostgresRepository struct{}

func NewUserPostgresRepository() *UserPostgresRepository { return &UserPostgresRepository{} }

func (r *UserPostgresRepository) Create(tx sharedDomain.Transaction, u *userDomain.User) (*userDomain.User, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := userToModel(u)
	if err := gormTx.Create(&m).Error; err != nil {
		return nil, err
	}
	return userToDomain(&m), nil
}

func (r *UserPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*userDomain.User, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var m models.UserModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(userErrors.ErrUserNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return userToDomain(&m), nil
}

func (r *UserPostgresRepository) GetByEmail(tx sharedDomain.Transaction, email string) (*userDomain.User, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var m models.UserModel
	err := gormTx.Where("LOWER(email) = ? AND deleted_at IS NULL", strings.ToLower(email)).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(userErrors.ErrUserNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return userToDomain(&m), nil
}

func (r *UserPostgresRepository) ExistsByEmail(tx sharedDomain.Transaction, email string) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	clean := strings.ToLower(strings.TrimSpace(email))
	if clean == "" {
		return false, nil
	}
	var n int64
	err := gormTx.Model(&models.UserModel{}).
		Where("LOWER(email) = ? AND deleted_at IS NULL", clean).
		Count(&n).Error
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *UserPostgresRepository) ExistsByPhoneInGym(tx sharedDomain.Transaction, gymID uuid.UUID, phone string, excludeUserID *uuid.UUID) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	normalized := userDomain.NormalizePhone(phone)
	if normalized == "" {
		return false, nil
	}
	q := gormTx.Model(&models.UserModel{}).
		Where("gym_id = ? AND phone = ? AND deleted_at IS NULL", gymID, normalized)
	if excludeUserID != nil {
		q = q.Where("id <> ?", *excludeUserID)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *UserPostgresRepository) Update(tx sharedDomain.Transaction, u *userDomain.User) (*userDomain.User, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	u.UpdatedAt = time.Now().UTC()
	m := userToModel(u)
	if err := gormTx.Where("id = ?", u.ID).
		Omit("id", "created_at").
		Save(&m).Error; err != nil {
		return nil, err
	}
	return userToDomain(&m), nil
}

func (r *UserPostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*userDomain.User, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.UserModel
	if err := gormTx.Where("gym_id = ? AND deleted_at IS NULL", gymID).
		Order("created_at ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*userDomain.User, len(rows))
	for i := range rows {
		out[i] = userToDomain(&rows[i])
	}
	return out, nil
}

func (r *UserPostgresRepository) CountOperatorsByGym(tx sharedDomain.Transaction, gymID uuid.UUID) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.UserModel{}).
		Where("gym_id = ? AND role = ? AND deleted_at IS NULL", gymID, userDomain.RoleOperator).
		Count(&n).Error
	return int(n), err
}

// Mappers

func userToModel(u *userDomain.User) models.UserModel {
	return models.UserModel{
		ID:                 u.ID,
		GymID:              u.GymID,
		Version:            u.Version,
		CreatedAt:          u.CreatedAt,
		UpdatedAt:          u.UpdatedAt,
		DeletedAt:          u.DeletedAt,
		Email:              u.Email,
		PasswordHash:       u.PasswordHash,
		FullName:           u.FullName,
		Phone:              u.Phone,
		Role:               u.Role,
		Active:             u.Active,
		MustChangePassword: u.MustChangePassword,
		LastLoginAt:        u.LastLoginAt,
		CreatedBy:          u.CreatedBy,
		PinHash:            u.PinHash,
		PinAssignedAt:      u.PinAssignedAt,
	}
}

func userToDomain(m *models.UserModel) *userDomain.User {
	return &userDomain.User{
		ID:                 m.ID,
		GymID:              m.GymID,
		Version:            m.Version,
		Email:              m.Email,
		PasswordHash:       m.PasswordHash,
		FullName:           m.FullName,
		Phone:              m.Phone,
		Role:               m.Role,
		Active:             m.Active,
		MustChangePassword: m.MustChangePassword,
		LastLoginAt:        m.LastLoginAt,
		CreatedBy:          m.CreatedBy,
		CreatedAt:          m.CreatedAt,
		UpdatedAt:          m.UpdatedAt,
		DeletedAt:          m.DeletedAt,
		PinHash:            m.PinHash,
		PinAssignedAt:      m.PinAssignedAt,
	}
}
