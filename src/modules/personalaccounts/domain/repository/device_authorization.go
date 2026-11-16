package repository

import (
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type DeviceAuthorizationRepository interface {
	Create(shared.Transaction, attempt.Binding, *deviceauthorization.Authorization) error
	GetForAttempt(shared.Transaction, uuid.UUID, attempt.Binding) (*deviceauthorization.Authorization, error)
	GetForActor(shared.Transaction, security.AccessClaims, uuid.UUID) (*deviceauthorization.Authorization, attempt.Binding, error)
	SaveDecision(shared.Transaction, security.AccessClaims, *deviceauthorization.Authorization) error
	StreamEpoch(shared.Transaction, security.AccessClaims) (uuid.UUID, error)
	List(shared.Transaction, security.AccessClaims, *deviceauthorization.ListPosition, int) ([]deviceauthorization.Authorization, error)
}
