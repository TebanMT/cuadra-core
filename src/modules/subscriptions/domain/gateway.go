package domain

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// CheckoutRequest is the provider-agnostic input the StartCheckout use case
// hands to whichever gateway the gym owner picked. The gym name + customer
// email are not strictly required by every processor, but Stripe surfaces them
// in receipts and MP uses the email as the payer hint.
type CheckoutRequest struct {
	GymID         uuid.UUID
	Plan          string // gym domain plan code: "standard_monthly" / "standard_annual" / "plus_monthly" / "plus_annual"
	GymName       string
	CustomerEmail string
	SuccessURL    string
	CancelURL     string
}

// CheckoutResult is what we hand back to the FE so it can redirect the
// browser. SessionID / PreapprovalID is informational — the webhook is the
// authoritative state-transition signal.
type CheckoutResult struct {
	URL       string
	SessionID string
}

// CheckoutGateway is implemented per processor (Stripe Checkout Session,
// Mercado Pago Preapproval). The use case picks one based on the requested
// provider; the gateway encapsulates the SDK / HTTP call.
type CheckoutGateway interface {
	Provider() Provider
	StartCheckout(ctx context.Context, in CheckoutRequest) (CheckoutResult, error)
}

// ErrGatewayUnavailable is returned by the use case when the gym requested a
// provider that is not configured (e.g. Mercado Pago when MP_ACCESS_TOKEN is
// missing). The HTTP layer maps this to 503.
var ErrGatewayUnavailable = errors.New("payment gateway not configured")

// ErrUnsupportedPlan is returned by a gateway when the plan code has no
// corresponding price/preapproval id configured on its side.
var ErrUnsupportedPlan = errors.New("plan not configured for this gateway")
