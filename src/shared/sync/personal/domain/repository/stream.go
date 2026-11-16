package repository

import (
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

// Caller owns the short account transaction and checks the live actor first.
// Groups are complete, ordered and bounded; no method commits independently.
type Stream interface {
	State(shared.Transaction, uuid.UUID) (protocol.StreamState, error)
	Groups(shared.Transaction, uuid.UUID, int64, int) ([]protocol.ChangeGroup, error)
}
