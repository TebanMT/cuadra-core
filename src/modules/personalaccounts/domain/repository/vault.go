package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/vault"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// VaultRepository accepts an existing scoped auth transaction. Creation never
// replaces an existing vault or its key; unwrapping remains an application step.
type VaultRepository interface {
	Create(tx shared.Transaction, accountID uuid.UUID, v *vault.Vault) error
	Get(tx shared.Transaction, accountID, vaultID uuid.UUID) (*vault.Vault, error)
}
