package domain

import (
	"errors"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ErrDuplicateEvent is returned by EventRepository.Insert when a row with
// (provider, external_id) already exists. The use case treats this as a
// successful idempotent replay (Stripe retries on non-2xx).
var ErrDuplicateEvent = errors.New("subscription event already recorded")

// EventRepository persists `subscription_events`. Cloud-only — sidecars
// never touch billing.
type EventRepository interface {
	// Insert appends a new event. Implementations MUST treat (provider,
	// external_id) as a unique key — duplicate webhooks resolve to no-op
	// (return ErrDuplicateEvent).
	Insert(tx sharedDomain.Transaction, e *Event) error
	// ListByGym returns the most recent `limit` events for the gym, newest
	// first. Used by the settings page "historial de pagos".
	ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]*Event, error)
	// ExistsByExternalID is the idempotency check the webhook controller
	// performs before applying the mutation.
	ExistsByExternalID(tx sharedDomain.Transaction, provider Provider, externalID string) (bool, error)
}
