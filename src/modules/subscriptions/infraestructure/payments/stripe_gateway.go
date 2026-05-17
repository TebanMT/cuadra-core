// Package payments holds the concrete CheckoutGateway adapters. Each
// processor lives in its own file so that swapping (or removing) one is
// confined to a single place — the use case only knows about the domain
// interface.
package payments

import (
	"context"
	"errors"
	"fmt"

	stripe "github.com/stripe/stripe-go/v82"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
)

// StripeConfig holds the bits the Stripe Checkout adapter needs from env. The
// price ids are created in the Stripe dashboard (Products → Add price) and
// pasted here. Locale is fixed to "es-419" so MX customers see Spanish copy
// even on the Stripe-hosted page.
type StripeConfig struct {
	SecretKey     string
	PriceStandard string // monthly $799 MXN — maps to gym plan "standard_monthly"
	// PricePlus mapea al plan "plus_monthly" a $1,199 MXN. Plus aún no
	// se vende públicamente (espera a que app del socio + tap-to-sell +
	// WhatsApp completo entren en producción), pero el price ID se puede
	// pre-crear en Stripe para tenerlo listo el día del lanzamiento.
	PricePlus string
}

// StripeGateway implements subDomain.CheckoutGateway by calling the Stripe
// SDK's `/v1/checkout/sessions` endpoint in mode=subscription.
type StripeGateway struct {
	client *stripe.Client
	prices map[string]string // gym plan code → Stripe price_id
}

// NewStripeGateway returns nil when SecretKey is empty so the wiring code
// can register the gateway only when Stripe is actually configured. The use
// case treats a missing entry in its provider map as ErrGatewayUnavailable.
func NewStripeGateway(cfg StripeConfig) *StripeGateway {
	if cfg.SecretKey == "" {
		return nil
	}
	prices := map[string]string{}
	if cfg.PriceStandard != "" {
		prices[gymDomain.PlanStandardMonthly] = cfg.PriceStandard
	}
	if cfg.PricePlus != "" {
		prices[gymDomain.PlanPlusMonthly] = cfg.PricePlus
	}
	return &StripeGateway{
		client: stripe.NewClient(cfg.SecretKey),
		prices: prices,
	}
}

func (g *StripeGateway) Provider() subDomain.Provider { return subDomain.ProviderStripe }

// StartCheckout creates a Stripe Checkout Session in subscription mode and
// returns its hosted URL. We pin the gym id into both `metadata.gym_id` and
// `client_reference_id` so the webhook side (which already inspects both,
// see parseStripeEvent) can resolve the gym regardless of which event arrives
// first (`checkout.session.completed` vs. `customer.subscription.created`).
func (g *StripeGateway) StartCheckout(ctx context.Context, in subDomain.CheckoutRequest) (subDomain.CheckoutResult, error) {
	priceID, ok := g.prices[in.Plan]
	if !ok || priceID == "" {
		return subDomain.CheckoutResult{}, fmt.Errorf("stripe: %w (plan=%s)", subDomain.ErrUnsupportedPlan, in.Plan)
	}
	gymRef := in.GymID.String()
	params := &stripe.CheckoutSessionCreateParams{
		Mode:              stripe.String("subscription"),
		SuccessURL:        stripe.String(in.SuccessURL),
		CancelURL:         stripe.String(in.CancelURL),
		ClientReferenceID: stripe.String(gymRef),
		Locale:            stripe.String("es-419"),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{
				Price:    stripe.String(priceID),
				Quantity: stripe.Int64(1),
			},
		},
		// Subscription created by the session inherits this metadata, so the
		// renewal webhooks (`invoice.paid`, `customer.subscription.deleted`)
		// also carry gym_id without us having to reconcile by customer id.
		SubscriptionData: &stripe.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: map[string]string{"gym_id": gymRef},
		},
		Metadata:            map[string]string{"gym_id": gymRef},
		AllowPromotionCodes: stripe.Bool(true),
	}
	if in.CustomerEmail != "" {
		params.CustomerEmail = stripe.String(in.CustomerEmail)
	}

	sess, err := g.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return subDomain.CheckoutResult{}, fmt.Errorf("stripe: create checkout session: %w", err)
	}
	if sess == nil || sess.URL == "" {
		return subDomain.CheckoutResult{}, errors.New("stripe: empty session url")
	}
	return subDomain.CheckoutResult{URL: sess.URL, SessionID: sess.ID}, nil
}
