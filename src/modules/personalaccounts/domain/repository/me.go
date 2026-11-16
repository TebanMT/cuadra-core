package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/me"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// MeRepository returns a single snapshot, gated by the session in that same
// database statement. The authenticated actor is supplied by the application.
type MeRepository interface {
	Get(tx shared.Transaction, accountID, sessionID, deviceID uuid.UUID) (*me.Snapshot, error)
}
