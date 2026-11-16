//go:build server

package repositories

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/recovery"
	port "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RecoveryKeyPostgresRepository struct{}

func NewRecoveryKeyRepository() *RecoveryKeyPostgresRepository {
	return &RecoveryKeyPostgresRepository{}
}

var _ port.RecoveryKeyRepository = (*RecoveryKeyPostgresRepository)(nil)

func (*RecoveryKeyPostgresRepository) Create(handle shared.Transaction, accountID uuid.UUID, key *recovery.RecoveryKey) (err error) {
	defer func() { err = recoveryPersistenceError(err) }()
	if err = key.Validate(); err != nil {
		return err
	}
	if key.AccountID != accountID || key.PreparedAt != nil || key.ConsumedAt != nil || key.RevokedAt != nil {
		return personalErrors.ErrInvalidRecoveryKey
	}
	tx, err := recoveryConnection(handle, accountID, true)
	if err != nil {
		return err
	}
	row := recoveryToModel(key)
	// The unique live-key index and (account,generation) constraint are final
	// defenses. No conflict path overwrites an ID, verifier or prior generation.
	return tx.Create(&row).Error
}

func (*RecoveryKeyPostgresRepository) Get(handle shared.Transaction, accountID, keyID uuid.UUID) (key *recovery.RecoveryKey, err error) {
	defer func() { err = recoveryPersistenceError(err) }()
	tx, err := recoveryConnection(handle, accountID, false)
	if err != nil {
		return nil, err
	}
	var row models.RecoveryKeyModel
	if err = tx.Where("account_id=? AND id=?", accountID, keyID).Take(&row).Error; err != nil {
		return nil, err
	}
	return recoveryToDomain(row)
}

func (*RecoveryKeyPostgresRepository) GetLive(handle shared.Transaction, accountID uuid.UUID) (key *recovery.RecoveryKey, err error) {
	defer func() { err = recoveryPersistenceError(err) }()
	tx, err := recoveryConnection(handle, accountID, false)
	if err != nil {
		return nil, err
	}
	var row models.RecoveryKeyModel
	if err = tx.Where("account_id=? AND consumed_at IS NULL AND revoked_at IS NULL", accountID).Take(&row).Error; err != nil {
		return nil, err
	}
	return recoveryToDomain(row)
}

func (*RecoveryKeyPostgresRepository) SaveState(handle shared.Transaction, accountID uuid.UUID, key *recovery.RecoveryKey) (err error) {
	defer func() { err = recoveryPersistenceError(err) }()
	if err = key.Validate(); err != nil {
		return err
	}
	if key.AccountID != accountID {
		return personalErrors.ErrInvalidRecoveryKey
	}
	tx, err := recoveryConnection(handle, accountID, true)
	if err != nil {
		return err
	}
	var row models.RecoveryKeyModel
	if err = tx.Where("account_id=? AND id=?", accountID, key.ID).Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row).Error; err != nil {
		return err
	}
	current, err := recoveryToDomain(row)
	if err != nil {
		return err
	}
	if current.ID != key.ID || current.AccountID != key.AccountID || current.Generation != key.Generation || current.Verifier.KeyVersion != key.Verifier.KeyVersion || subtle.ConstantTimeCompare(current.Verifier.Digest, key.Verifier.Digest) != 1 || !current.CreatedAt.Equal(key.CreatedAt) {
		return personalErrors.ErrInvalidRecoveryKey
	}
	if !recoveryStampPreserved(current.PreparedAt, key.PreparedAt) || !recoveryStampPreserved(current.ConsumedAt, key.ConsumedAt) || !recoveryStampPreserved(current.RevokedAt, key.RevokedAt) {
		return personalErrors.ErrRecoveryKeyState
	}
	if !current.Live() && (!sameRecoveryStamp(current.PreparedAt, key.PreparedAt) || !sameRecoveryStamp(current.ConsumedAt, key.ConsumedAt) || !sameRecoveryStamp(current.RevokedAt, key.RevokedAt)) {
		return personalErrors.ErrRecoveryKeyState
	}
	if sameRecoveryStamp(current.PreparedAt, key.PreparedAt) && sameRecoveryStamp(current.ConsumedAt, key.ConsumedAt) && sameRecoveryStamp(current.RevokedAt, key.RevokedAt) {
		return nil
	}
	result := tx.Model(&models.RecoveryKeyModel{}).Where("account_id=? AND id=? AND prepared_at IS NOT DISTINCT FROM ? AND consumed_at IS NOT DISTINCT FROM ? AND revoked_at IS NOT DISTINCT FROM ?", accountID, key.ID, current.PreparedAt, current.ConsumedAt, current.RevokedAt).
		Updates(map[string]any{"prepared_at": key.PreparedAt, "consumed_at": key.ConsumedAt, "revoked_at": key.RevokedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return personalErrors.ErrRecoveryKeyState
	}
	return nil
}

func recoveryConnection(handle shared.Transaction, accountID uuid.UUID, write bool) (*gorm.DB, error) {
	tx, err := authSessionConnection(handle, accountID, write)
	if err != nil {
		return nil, err
	}
	var active bool
	if err = tx.Raw("SELECT EXISTS(SELECT 1 FROM mancuerna.accounts WHERE id=? AND status='active' AND deleted_at IS NULL)", accountID).Scan(&active).Error; err != nil {
		return nil, err
	}
	if !active {
		return nil, personalErrors.ErrRecoveryKeyNotFound
	}
	return tx, nil
}

func sameRecoveryStamp(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

func recoveryStampPreserved(a, b *time.Time) bool { return a == nil || sameRecoveryStamp(a, b) }

func recoveryPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{personalErrors.ErrInvalidRecoveryKey, personalErrors.ErrRecoveryKeyNotFound, personalErrors.ErrRecoveryKeyConflict, personalErrors.ErrRecoveryKeyState, personalErrors.ErrAuthSessionScope, personalErrors.ErrAuthObjectUnavailable, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return personalErrors.ErrRecoveryKeyNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return personalErrors.ErrRecoveryKeyConflict
	}
	return shared.NewUnexpectedError(errors.New("no se pudo persistir o consultar el kit de recuperación"))
}

func recoveryToModel(k *recovery.RecoveryKey) models.RecoveryKeyModel {
	return models.RecoveryKeyModel{ID: k.ID, AccountID: k.AccountID, SecretVerifier: append([]byte(nil), k.Verifier.Digest...), VerifierKeyVersion: k.Verifier.KeyVersion, Generation: k.Generation, CreatedAt: k.CreatedAt.UTC(), PreparedAt: utcPointer(k.PreparedAt), ConsumedAt: utcPointer(k.ConsumedAt), RevokedAt: utcPointer(k.RevokedAt)}
}

func recoveryToDomain(row models.RecoveryKeyModel) (*recovery.RecoveryKey, error) {
	k := &recovery.RecoveryKey{ID: row.ID, AccountID: row.AccountID, Verifier: security.RecoveryVerifier{KeyVersion: row.VerifierKeyVersion, Digest: append([]byte(nil), row.SecretVerifier...)}, Generation: row.Generation, CreatedAt: row.CreatedAt.UTC(), PreparedAt: utcPointer(row.PreparedAt), ConsumedAt: utcPointer(row.ConsumedAt), RevokedAt: utcPointer(row.RevokedAt)}
	return k, k.Validate()
}
