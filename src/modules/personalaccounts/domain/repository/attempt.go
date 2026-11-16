package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/shared/domain"
)

// AuthAttemptRepository never opens or commits a transaction. Bind each call
// to the original client attempt ID and SHA-256 continuation credential.
type AuthAttemptRepository interface {
	Create(domain.Transaction, *attempt.AuthAttempt) error
	Get(domain.Transaction, attempt.Binding) (*attempt.AuthAttempt, error)
	GetForUpdate(domain.Transaction, attempt.Binding) (*attempt.AuthAttempt, error)
	Save(domain.Transaction, attempt.Binding, *attempt.AuthAttempt, int64) error
}
