package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Resolves only a phone-change initiator whose attempt continuation is already
// scoped in this transaction. These internal IDs are never an HTTP result.
type PhoneChangeAttemptResolver interface {
	Resolve(shared.Transaction, attempt.Binding) (security.AccessClaims, error)
}
