package repository

import (
	"context"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deletion"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type DeletionRepository interface {
	// Request uses the caller's account mutex and requires the exact consumed
	// proof. It blocks access and revokes sessions, never purges data or keys.
	Request(shared.Transaction, security.AccessClaims, deletion.Request) error
	// GetResult returns only a committed acceptance, even without live access.
	GetResult(shared.Transaction, uuid.UUID, uuid.UUID, [32]byte) (deletion.Result, error)
}
type DeletionResultResolver interface {
	// Resolve is a fixed auth-only lookup, not account authorization. GetResult
	// must recheck operation, hash and owner in the resulting account scope.
	Resolve(context.Context, [32]byte) (uuid.UUID, error)
}
