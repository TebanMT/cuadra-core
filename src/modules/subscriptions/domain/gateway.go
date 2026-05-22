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
//
// PaymentMethod elige el método de pago dentro del provider:
//   - "" o "card" → comportamiento default (Stripe Subscriptions con tarjeta,
//     MP preapproval con tarjeta).
//   - "oxxo"      → SÓLO Stripe + plan anual. Crea Checkout Session con
//     mode=payment (one-time) + payment_method_types=["oxxo"]. La activación
//     llega vía payment_intent.succeeded cuando el cliente paga la ficha
//     en tienda. Stripe NO soporta OXXO en mode=subscription, por eso el
//     gateway diverge en este caso.
type CheckoutRequest struct {
	GymID         uuid.UUID
	Plan          string // gym domain plan code: "standard_monthly" / "standard_annual" / "plus_monthly" / "plus_annual"
	GymName       string
	CustomerEmail string
	SuccessURL    string
	CancelURL     string
	PaymentMethod string
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

// BillingPortalRequest is the input for opening a self-service portal where
// the customer can update payment method, see invoices, change plan, or
// cancel without going through Tinta support. CustomerID is the
// provider-side id (Stripe: cus_*, MP: not supported today — MP returns
// ErrBillingPortalUnsupported).
type BillingPortalRequest struct {
	CustomerID string
	ReturnURL  string
}

// BillingPortalResult is the hosted URL the FE redirects the browser to.
type BillingPortalResult struct {
	URL string
}

// BillingPortalGateway is implemented by gateways that expose a hosted
// customer portal. Today only Stripe supports this; MP customers manage
// their preapproval via their own MP account UI.
type BillingPortalGateway interface {
	StartBillingPortal(ctx context.Context, in BillingPortalRequest) (BillingPortalResult, error)
}

// ErrBillingPortalUnsupported — devuelto cuando el gateway no expone portal
// (MP). El use case lo traduce a 409 con copy "tu suscripción la administras
// desde Mercado Pago".
var ErrBillingPortalUnsupported = errors.New("billing portal not supported by this gateway")

// ErrGatewayUnavailable is returned by the use case when the gym requested a
// provider that is not configured (e.g. Mercado Pago when MP_ACCESS_TOKEN is
// missing). The HTTP layer maps this to 503.
var ErrGatewayUnavailable = errors.New("payment gateway not configured")

// ErrUnsupportedPlan is returned by a gateway when the plan code has no
// corresponding price/preapproval id configured on its side.
var ErrUnsupportedPlan = errors.New("plan not configured for this gateway")
