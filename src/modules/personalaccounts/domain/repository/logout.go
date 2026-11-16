package repository

import (
	"context"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// A logout ticket grants only revocation of its exact session. This port has
// no actor-authentication or general identity-lookup capability.
type LogoutRepository interface {
	Resolve(context.Context, [32]byte) (uuid.UUID, error)
	GetSessionForTicket(shared.Transaction, uuid.UUID, [32]byte) (*session.Session, error)
}
