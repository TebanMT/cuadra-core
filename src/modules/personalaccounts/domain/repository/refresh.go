package repository

import (
	"context"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/refresh"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// RefreshIdentityResolver has no arbitrary lookup discriminator or SQL. The
// account ID is only a hint: token, signature and session are rechecked in tx.
type RefreshIdentityResolver interface {
	Resolve(context.Context, [32]byte) (uuid.UUID, error)
}
type RefreshSessionRepository interface {
	GetForUpdate(shared.Transaction, uuid.UUID, [32]byte) (*session.Session, *refresh.Rotation, error)
	SaveRotation(shared.Transaction, uuid.UUID, *session.Session, *refresh.Rotation) error
}
