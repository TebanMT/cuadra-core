package domain

import (
	"errors"
	"time"

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
	// LatestOccurredAtForGym returns the highest occurred_at among events
	// previously applied to this gym. nil if the gym has no events yet.
	// RecordEvent uses this to reject out-of-order webhooks (a retry of an
	// older event arriving after a newer state transition would otherwise
	// regress the gym to a stale state).
	LatestOccurredAtForGym(tx sharedDomain.Transaction, gymID uuid.UUID) (*time.Time, error)
}
