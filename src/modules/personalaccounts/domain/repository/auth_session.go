package repository

import (
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type AuthSessionRepository interface {
	CreateDevice(shared.Transaction, uuid.UUID, *device.AuthorizedDevice) error
	CreateSession(shared.Transaction, uuid.UUID, *session.Session) error
	GetDevice(shared.Transaction, uuid.UUID, uuid.UUID) (*device.AuthorizedDevice, error)
	GetSession(shared.Transaction, uuid.UUID, uuid.UUID) (*session.Session, error)
	// SessionExpired proves expiry against the persistence clock while holding
	// the account mutex. Other reasons for SessionActive=false are not expiry.
	SessionExpired(shared.Transaction, uuid.UUID, uuid.UUID) (bool, error)
	RevokeDevice(shared.Transaction, uuid.UUID, uuid.UUID, time.Time, string) error
	RevokeSession(shared.Transaction, uuid.UUID, uuid.UUID, time.Time) error
	SessionActive(shared.Transaction, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
}
