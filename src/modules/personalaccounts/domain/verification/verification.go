// Package verification defines the external phone-possession boundary.
// Account authorization, persisted budgets, attempts and idempotency remain
// application responsibilities; provider approval alone never creates access.
package verification

import (
	"context"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
)

type Channel string

const (
	SMS      Channel = "sms"
	WhatsApp Channel = "whatsapp"
)

type State string

const (
	Pending           State = "pending"
	Approved          State = "approved"
	Cancelled         State = "cancelled"
	Expired           State = "expired"
	AttemptsExhausted State = "attempts_exhausted"
	Failed            State = "failed"
)

type StartRequest struct {
	DestinationE164 string
	Channel         Channel
}

// Binding is persisted on the original application attempt. Check and Fetch
// must not reconstruct it from a different phone, candidate or user request.
type Binding struct {
	Reference       string
	DestinationE164 string
}

type CheckRequest struct {
	Binding
	Code string
}

// Result contains provider facts, not message delivery or account access.
// ExpiresAt is provider CreatedAt + the explicitly configured service validity.
// Reusing a pending provider verification must not extend its original expiry.
// The application additionally caps its own attempt to at most ten minutes.
type Result struct {
	Reference string
	Channel   Channel
	State     State
	CreatedAt time.Time
	ExpiresAt time.Time
}

type VerificationProvider interface {
	// Start performs exactly one send request. An explicit resend or channel
	// fallback requires a new application budget reservation before this call.
	Start(context.Context, StartRequest) (Result, error)
	// Check binds the code to the persisted provider reference and destination.
	Check(context.Context, CheckRequest) (Result, error)
	// Fetch only reads the known reference; it never sends a code. A missing
	// reference does not prove approval, expiration or any other terminal state.
	Fetch(context.Context, Binding) (Result, error)
}

type FailureKind string

const (
	InvalidConfiguration FailureKind = "verification_configuration"
	InvalidInput         FailureKind = "verification_input"
	Uncertain            FailureKind = "verification_uncertain"
	Unavailable          FailureKind = "verification_unavailable"
	Missing              FailureKind = "verification_missing"
	RateLimited          FailureKind = "verification_rate_limited"
	CheckLimitReached    FailureKind = "verification_check_limit"
	Rejected             FailureKind = "verification_rejected"
)

// Failure deliberately excludes upstream messages, URLs, bodies and wrapped
// network errors. Its metadata is safe for structured diagnostics. RetryAfter
// is provider advice, never permission to automatically repeat a send/check.
type Failure struct {
	Kind         FailureKind
	HTTPStatus   int
	ProviderCode int
	RetryAfter   time.Duration
}

func (e *Failure) Error() string { return string(e.Kind) }

func (e *Failure) Unwrap() error {
	switch e.Kind {
	case InvalidConfiguration:
		return personalErrors.ErrVerificationConfiguration
	case InvalidInput:
		return personalErrors.ErrVerificationInput
	case Uncertain:
		return personalErrors.ErrVerificationUncertain
	case Unavailable:
		return personalErrors.ErrVerificationUnavailable
	case Missing:
		return personalErrors.ErrVerificationMissing
	case RateLimited:
		return personalErrors.ErrVerificationRateLimited
	case CheckLimitReached:
		return personalErrors.ErrVerificationCheckLimit
	case Rejected:
		return personalErrors.ErrVerificationRejected
	default:
		return nil
	}
}
