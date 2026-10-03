//go:build server

package repositories

import (
	"encoding/json"
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
	if err := emitUser(gormTx, u); err != nil {
		return nil, err
	}
	m.Version = u.Version
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
	if err := emitUser(gormTx, u); err != nil {
		return nil, err
	}
	m.Version = u.Version
	return userToDomain(&m), nil
}

// Cloud staff changes must reach a paired desktop before it can authenticate
// the operator offline. Publish in the same transaction as the canonical user;
// failing to publish must roll back the change instead of leaving a hidden user.
func emitUser(g *gorm.DB, u *userDomain.User) error {
	ms := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return t.UnixMilli()
	}
	payload, err := json.Marshal(map[string]any{
		"id": u.ID, "gym_id": u.GymID, "version": u.Version,
		"created_at": u.CreatedAt.UnixMilli(), "updated_at": u.UpdatedAt.UnixMilli(), "deleted_at": ms(u.DeletedAt),
		"email": u.Email, "password_hash": u.PasswordHash, "full_name": u.FullName,
		"phone": u.Phone, "role": u.Role, "active": u.Active, "must_change_password": u.MustChangePassword,
		"last_login_at": ms(u.LastLoginAt), "created_by": u.CreatedBy,
		"pin_hash": u.PinHash, "pin_assigned_at": ms(u.PinAssignedAt),
	})
	if err != nil {
		return err
	}
	var version int
	err = g.Raw(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at,deleted_at)
		VALUES(?,'users',?,?,?::jsonb,clock_timestamp(),?)
		ON CONFLICT(gym_id,entity_type,entity_id) DO UPDATE SET
		version=GREATEST(sync_entities.version+1,EXCLUDED.version),
		payload=jsonb_set(EXCLUDED.payload,'{version}',to_jsonb(GREATEST(sync_entities.version+1,EXCLUDED.version))),
		server_updated_at=clock_timestamp(),deleted_at=EXCLUDED.deleted_at RETURNING version`,
		u.GymID, u.ID, u.Version, string(payload), u.DeletedAt).Scan(&version).Error
	if err != nil {
		return err
	}
	if version != u.Version {
		if err := g.Exec(`UPDATE users SET version=? WHERE gym_id=? AND id=?`, version, u.GymID, u.ID).Error; err != nil {
			return err
		}
		u.Version = version
	}
	return nil
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
		EmailVerifiedAt:    u.EmailVerifiedAt,
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
		EmailVerifiedAt:    m.EmailVerifiedAt,
	}
}
