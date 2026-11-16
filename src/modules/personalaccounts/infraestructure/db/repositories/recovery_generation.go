//go:build server

package repositories

import (
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"math"
)

func (*RecoveryKeyPostgresRepository) NextGeneration(handle shared.Transaction, accountID uuid.UUID) (generation int64, err error) {
	defer func() { err = recoveryPersistenceError(err) }()
	tx, err := recoveryConnection(handle, accountID, true)
	if err != nil {
		return 0, err
	}
	var latest int64
	if err = tx.Raw("SELECT coalesce(max(generation),0) FROM mancuerna.recovery_keys WHERE account_id=?", accountID).Scan(&latest).Error; err != nil {
		return 0, err
	}
	if latest < 0 || latest == math.MaxInt64 {
		return 0, personalErrors.ErrRecoveryKeyState
	}
	return latest + 1, nil
}
