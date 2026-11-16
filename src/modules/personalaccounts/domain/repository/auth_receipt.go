package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type AuthReceiptRepository interface {
	Get(shared.Transaction, receipt.Owner, uuid.UUID) (*receipt.Receipt, error)
	Create(shared.Transaction, *receipt.Receipt) error
	// SaveResponse updates only encrypted response fields for the same owner,
	// operation, purpose and hash; it cannot rewrite the effect identity.
	SaveResponse(shared.Transaction, *receipt.Receipt) error
}
