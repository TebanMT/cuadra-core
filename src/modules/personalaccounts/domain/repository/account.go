package repository

import (
	accountDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// AccountRepository operates inside the caller's personal transaction. ownerID
// comes from the authenticated context, never from the profile request body.
// UpdateProfile must be composed with a receipt/revision/journal write before
// committing; the repository does not commit or start a separate transaction.
type AccountRepository interface {
	GetByID(tx sharedDomain.Transaction, ownerID uuid.UUID) (*accountDomain.Account, error)
	UpdateProfile(tx sharedDomain.Transaction, ownerID uuid.UUID, updated *accountDomain.Account, expectedRevision int64) (*accountDomain.Account, error)
}
