package repository

import (
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// BoundAccess identifies the account while its mutation mutex is held.
// Created=false requires separate device authorization or recovery. Neither
// result grants a session or authorizes the candidate device by itself.
type BoundAccess struct {
	AccountID uuid.UUID
	Created   bool
}

type IdentityAccessRepository interface {
	// ResolveVerifiedAccess transitions one verified access attempt into an
	// account scope in the existing transaction. The caller must propagate
	// errors and persist device/session/attempt changes in the same command.
	ResolveVerifiedAccess(shared.Transaction, uuid.UUID) (BoundAccess, error)
}
