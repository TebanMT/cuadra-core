//go:build server

package repositories

import (
	"context"
	"errors"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/vault"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

type VaultPostgresRepository struct{}

func NewVaultRepository() *VaultPostgresRepository { return &VaultPostgresRepository{} }

var _ repository.VaultRepository = (*VaultPostgresRepository)(nil)

func vaultConnection(handle shared.Transaction, accountID uuid.UUID, write bool) (*gorm.DB, error) {
	tx, err := authSessionConnection(handle, accountID, write)
	if err != nil {
		return nil, err
	}
	var pure, active bool
	err = tx.Raw(`SELECT coalesce(current_setting('mancuerna.attempt_id',true),'')='' AND coalesce(current_setting('mancuerna.attempt_hash',true),'')=''`).Scan(&pure).Error
	if err != nil {
		return nil, err
	}
	if !pure {
		return nil, personalErrors.ErrAuthSessionScope
	}
	err = tx.Raw("SELECT EXISTS(SELECT 1 FROM mancuerna.accounts WHERE id=? AND status='active' AND deleted_at IS NULL)", accountID).Scan(&active).Error
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, personalErrors.ErrVaultNotFound
	}
	return tx, nil
}

func (*VaultPostgresRepository) Create(handle shared.Transaction, accountID uuid.UUID, v *vault.Vault) (err error) {
	defer func() { err = vaultPersistenceError(err) }()
	if v.Validate() != nil || v.AccountID != accountID || !v.Live() {
		return personalErrors.ErrVaultState
	}
	tx, err := vaultConnection(handle, accountID, true)
	if err != nil {
		return err
	}
	row := vaultToModel(v)
	// Deliberately no upsert: UUID collisions cannot change or return a key.
	return tx.Create(&row).Error
}

func (*VaultPostgresRepository) Get(handle shared.Transaction, accountID, vaultID uuid.UUID) (v *vault.Vault, err error) {
	defer func() { err = vaultPersistenceError(err) }()
	tx, err := vaultConnection(handle, accountID, false)
	if err != nil {
		return nil, err
	}
	var row models.VaultModel
	if err = tx.Where("account_id=? AND vault_id=?", accountID, vaultID).Take(&row).Error; err != nil {
		return nil, err
	}
	return vaultToDomain(row)
}

func vaultPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrVaultNotFound, personalErrors.ErrVaultConflict, personalErrors.ErrVaultState, personalErrors.ErrAuthSessionScope, personalErrors.ErrAuthObjectUnavailable, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return personalErrors.ErrVaultNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return personalErrors.ErrVaultConflict
	}
	return shared.NewUnexpectedError(errors.New("no se pudo consultar o guardar el almacén cifrado"))
}

func vaultToModel(v *vault.Vault) models.VaultModel {
	return models.VaultModel{ID: v.ID, AccountID: v.AccountID, WrappedDataKey: append([]byte(nil), v.Envelope.Ciphertext...), KeyVersion: v.Envelope.KeyVersion, CreatedAt: v.CreatedAt.UTC(), RetiredAt: utcPointer(v.RetiredAt)}
}

func vaultToDomain(row models.VaultModel) (*vault.Vault, error) {
	v := &vault.Vault{ID: row.ID, AccountID: row.AccountID, Envelope: security.Envelope{KeyVersion: row.KeyVersion, Ciphertext: append([]byte(nil), row.WrappedDataKey...)}, CreatedAt: row.CreatedAt.UTC(), RetiredAt: utcPointer(row.RetiredAt)}
	return v, v.Validate()
}
