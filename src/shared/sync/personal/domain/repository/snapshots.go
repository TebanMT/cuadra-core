package repository

import (
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

// Every method uses the caller's transaction. Materialize reads a bounded
// record cursor and writes immutable pages inside SnapshotCommand only.
type Snapshots interface {
	RequireActor(shared.Transaction, protocol.Actor, bool) error
	State(shared.Transaction, uuid.UUID, bool) (protocol.StreamState, error)
	Get(shared.Transaction, uuid.UUID, uuid.UUID, bool) (protocol.StoredSnapshot, error)
	Materialize(shared.Transaction, uuid.UUID, uuid.UUID) ([]string, error)
	Save(shared.Transaction, protocol.StoredSnapshot) error
	Page(shared.Transaction, uuid.UUID, uuid.UUID, int) (protocol.SnapshotPage, error)
}
