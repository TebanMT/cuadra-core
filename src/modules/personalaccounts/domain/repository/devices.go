package repository

import (
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// All methods use the caller's auth transaction and its account stream lock.
// Revoke includes session withdrawal and cancellation of dependent, unconsumed
// authorizations. It does not consume a proof, create a receipt, or commit.
type DevicesRepository interface {
	StreamEpoch(shared.Transaction, security.AccessClaims) (uuid.UUID, error)
	List(shared.Transaction, security.AccessClaims, *device.ListPosition, int) ([]device.ListedDevice, error)
	Revoke(shared.Transaction, security.AccessClaims, device.RevocationTarget, time.Time) error
}
