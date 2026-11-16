package repository

import (
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceproof"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type DeviceProofRepository interface {
	Create(shared.Transaction, security.AccessClaims, *deviceproof.Proof) error
	GetForUpdate(shared.Transaction, security.AccessClaims, uuid.UUID) (*deviceproof.Proof, error)
	SaveVerified(shared.Transaction, security.AccessClaims, *deviceproof.Proof) error
	// The caller checks its effect receipt after active actor validation and
	// before Consume. Any later error must roll back this transaction as well.
	Consume(shared.Transaction, security.AccessClaims, [32]byte, security.ProofPurpose, [32]byte, uuid.UUID, time.Time) error
}
