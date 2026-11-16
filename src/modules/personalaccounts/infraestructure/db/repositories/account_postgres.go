//go:build server

package repositories

import (
	"errors"
	"time"

	accountDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	personalRepository "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type AccountPostgresRepository struct{}

var _ personalRepository.AccountRepository = (*AccountPostgresRepository)(nil)

func NewAccountPostgresRepository() *AccountPostgresRepository { return &AccountPostgresRepository{} }

const profileColumns = "id,display_name,weight_unit,revision,status,created_at,updated_at,deleted_at"

func (r *AccountPostgresRepository) GetByID(tx sharedDomain.Transaction, ownerID uuid.UUID) (*accountDomain.Account, error) {
	if ownerID == uuid.Nil {
		return nil, sharedDomain.NewBusinessError(personalErrors.ErrAccountNotFound, "")
	}
	connection := tx.(*sharedDomain.GormTransaction).Tx
	var row models.AccountModel
	err := connection.Select(profileColumns).Where("id = ? AND deleted_at IS NULL", ownerID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(personalErrors.ErrAccountNotFound, "")
	}
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	account, err := toDomain(&row)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return account, nil
}

func (r *AccountPostgresRepository) UpdateProfile(tx sharedDomain.Transaction, ownerID uuid.UUID, updated *accountDomain.Account, expectedRevision int64) (*accountDomain.Account, error) {
	if updated == nil || ownerID == uuid.Nil || updated.ID != ownerID {
		return nil, sharedDomain.NewBusinessError(personalErrors.ErrAccountNotFound, "")
	}
	if err := updated.ValidateProfileWrite(expectedRevision); err != nil {
		if errors.Is(err, personalErrors.ErrAccountUnavailable) || errors.Is(err, personalErrors.ErrProfileRevisionExhausted) {
			return nil, sharedDomain.NewBusinessError(err, "")
		}
		return nil, sharedDomain.NewValidationError(err)
	}
	connection := tx.(*sharedDomain.GormTransaction).Tx
	row := toModel(updated)
	// An explicit column allowlist prevents profile writes from modifying phone,
	// credentials, status, timestamps of creation/deletion, or security version.
	result := connection.Model(&models.AccountModel{}).
		Where("id = ? AND revision = ? AND status = ? AND deleted_at IS NULL", ownerID, expectedRevision, string(accountDomain.Active)).
		Updates(map[string]any{"display_name": row.DisplayName, "weight_unit": row.WeightUnit, "revision": row.Revision, "updated_at": row.UpdatedAt})
	if result.Error != nil {
		return nil, sharedDomain.NewUnexpectedError(result.Error)
	}
	if result.RowsAffected == 0 {
		current, err := r.GetByID(tx, ownerID)
		if err != nil {
			return nil, err
		}
		if current.Status != accountDomain.Active {
			return nil, sharedDomain.NewBusinessError(personalErrors.ErrAccountUnavailable, "")
		}
		return nil, sharedDomain.NewBusinessError(personalErrors.ErrProfileRevisionConflict, "")
	}
	return r.GetByID(tx, ownerID)
}

func toModel(a *accountDomain.Account) models.AccountModel {
	return models.AccountModel{ID: a.ID, DisplayName: a.DisplayName, WeightUnit: string(a.WeightUnit), Revision: a.Revision, Status: string(a.Status), CreatedAt: a.CreatedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC(), DeletedAt: utcPointer(a.DeletedAt)}
}

func toDomain(row *models.AccountModel) (*accountDomain.Account, error) {
	a := &accountDomain.Account{ID: row.ID, DisplayName: row.DisplayName, WeightUnit: accountDomain.WeightUnit(row.WeightUnit), Revision: row.Revision, Status: accountDomain.Status(row.Status), CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(), DeletedAt: utcPointer(row.DeletedAt)}
	return a, a.Validate()
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
