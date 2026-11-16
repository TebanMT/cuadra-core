package repository

import (
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

// Commands never starts or commits a transaction. A caller holds the account
// stream lock and commits its business effect and this protocol metadata once.
type Commands interface {
	RequireActor(shared.Transaction, protocol.Actor, uuid.UUID) error
	GetReceipt(shared.Transaction, uuid.UUID, uuid.UUID) (*protocol.Receipt, error)
	SaveConflict(shared.Transaction, protocol.Conflict) error
	// CheckChanges validates the next concrete journal group without writing.
	// Keep this check and the business mutation in the same account transaction.
	CheckChanges(shared.Transaction, uuid.UUID, uuid.UUID, []protocol.Change) error
	Commit(shared.Transaction, protocol.Receipt, []protocol.Change) error
}
