package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"time"
)

// Internal resolution under both phone locks and the account mutex. These
// fields are not an authentication result and must not reach a public DTO.
type BoundRecovery struct {
	AccountID           uuid.UUID
	PhoneChangeRequired bool
	PhoneAvailable      bool
}

func (BoundRecovery) String() string     { return "recovery identity (binding redacted)" }
func (r BoundRecovery) GoString() string { return r.String() }

type RecoveryIdentityRepository interface {
	// Requires an attempt-only transaction and a directly verified recovery
	// OTP. Does not bind/consume the challenge or mutate credentials/account.
	ResolveVerifiedRecovery(shared.Transaction, attempt.Binding, uuid.UUID) (BoundRecovery, error)
	// Caller first verifies and consumes the kit in this same transaction.
	// All later failures must roll back the entire command.
	ApplyRecovery(shared.Transaction, attempt.Binding, uuid.UUID, uuid.UUID, bool, time.Time) error
}
