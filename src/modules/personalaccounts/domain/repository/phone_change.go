package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/phonechange"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/refresh"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"time"
)

type PhoneChangeRepository interface {
	CurrentPhone(shared.Transaction, security.AccessClaims) (string, error)
	// LockPhones runs before the account mutex, after any budget reservation.
	LockPhones(shared.Transaction, security.AccessClaims, string) (phonechange.Observation, error)
	ValidatePhones(shared.Transaction, security.AccessClaims, string, phonechange.Observation) error
	CheckInitiator(shared.Transaction, security.AccessClaims, *attempt.AuthAttempt) error
	// CheckProof only validates a token; Consume remains in the effect tx.
	CheckProof(shared.Transaction, security.AccessClaims, [32]byte, [32]byte, uuid.UUID, time.Time) error
	// Apply requires direct verified OTP, the initiating actor, consumed proof,
	// and the two phone locks. It never commits or exposes an arbitrary update.
	Apply(shared.Transaction, security.AccessClaims, attempt.Binding, phonechange.Observation, *session.Session, *refresh.Rotation, [32]byte, time.Time) error
}
