package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/recovery"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type RecoveryKeyRepository interface {
	Create(shared.Transaction, uuid.UUID, *recovery.RecoveryKey) error
	Get(shared.Transaction, uuid.UUID, uuid.UUID) (*recovery.RecoveryKey, error)
	GetLive(shared.Transaction, uuid.UUID) (*recovery.RecoveryKey, error)
	SaveState(shared.Transaction, uuid.UUID, *recovery.RecoveryKey) error
	// NextGeneration reads all historical keys under the account mutex. It
	// never recycles a consumed/revoked generation and makes no separate commit.
	NextGeneration(shared.Transaction, uuid.UUID) (int64, error)
}
