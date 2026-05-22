package app

import (
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// OXXORenewalCandidate is the projection consumed by the OXXO renewal cron.
// Carries the minimum to enqueue a notification + check WhatsApp readiness
// without the use case having to round-trip to the gym repo.
type OXXORenewalCandidate struct {
	GymID              uuid.UUID
	GymName            string
	GymWhatsAppReady   bool
	SubscriptionEndsAt time.Time
}

// OXXORenewalReader is the query surface used by RunOXXORenewalReminders +
// CancelExpiredOXXO. Both methods filter to gyms whose LAST activated or
// renewed event came from OXXO; tarjeta-anual gyms renew themselves via
// Stripe Subscriptions and never enter this path.
//
// Lives in the subscriptions BC (not gyms) because the OXXO marker is a
// subscription_events fact — the gyms table doesn't track payment_method.
type OXXORenewalReader interface {
	// FindRenewalCandidates returns annual+active OXXO gyms whose
	// SubscriptionEndsAt falls within [windowStart, windowEnd]. The window
	// is centered on `now + DaysBefore` with ±12h tolerance so a daily
	// cron run catches each stage once even with clock drift.
	FindRenewalCandidates(tx sharedDomain.Transaction, windowStart, windowEnd time.Time) ([]OXXORenewalCandidate, error)

	// FindExpiredOXXO returns annual+active OXXO gyms whose
	// SubscriptionEndsAt is strictly before `before` AND there is no
	// activated/renewed event recorded after their SubscriptionEndsAt.
	// Used by the post-expiry cancel job: caller passes `now - grace`.
	FindExpiredOXXO(tx sharedDomain.Transaction, before time.Time) ([]OXXORenewalCandidate, error)
}
