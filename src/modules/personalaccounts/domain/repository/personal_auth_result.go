package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// AccountAuthResult contains no continuation capability or challenge secret.
type AccountAuthResult struct {
	Receipt              *receipt.Receipt
	PhoneChangeCommitted bool
}

func (AccountAuthResult) String() string     { return "personal auth result receipt (ciphertext redacted)" }
func (r AccountAuthResult) GoString() string { return r.String() }

type PersonalAuthResultRepository interface {
	GetAccountResult(shared.Transaction, uuid.UUID, uuid.UUID) (AccountAuthResult, error)
}
