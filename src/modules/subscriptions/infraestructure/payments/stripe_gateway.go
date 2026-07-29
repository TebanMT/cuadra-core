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
	// PriceStandardAnnual mapea al plan "standard_annual" a $7,200 MXN/año
	// (recurring yearly en Stripe). El descuento de ~25% vs 12×mensual está
	// definido por producto (psicología pago-anual ≈ pago-único + cash flow
	// predecible). El SDK acepta el price recurrente anual con la misma
	// CheckoutSession `mode=subscription` — el interval se infiere del price.
	PriceStandardAnnual string
	// PriceStandardAnnualOXXO es un price one-time (NO recurring) por $7,200
	// MXN, usado exclusivamente para el flow de OXXO. Stripe no permite OXXO
	// en mode=subscription, así que la única forma de cobrar anual con
	// voucher OXXO es Checkout en mode=payment con un price one-time. La
	// activación del gym la hace Tinta del lado servidor cuando llega
	// payment_intent.succeeded (no hay Stripe Subscription que renueve sola
	// — el cron de Tinta envía un nuevo link de checkout 30 días antes del
	// vencimiento).
	PriceStandardAnnualOXXO string
	// PricePlus mapea al plan "plus_monthly". Plus aún no se vende
	// públicamente (espera a que app del socio + tap-to-sell + WhatsApp
	// completo entren en producción), pero el price ID se puede pre-crear
	// en Stripe para tenerlo listo el día del lanzamiento.
	PricePlus string
}

// StripeGateway implements subDomain.CheckoutGateway by calling the Stripe
// SDK's `/v1/checkout/sessions` endpoint. Default flow is mode=subscription;
// the OXXO branch switches to mode=payment with a one-time price (Stripe
// limitation, not ours).
type StripeGateway struct {
	client            *stripe.Client
	prices            map[string]string // gym plan code → Stripe price_id (recurring)
	oxxoAnnualPriceID string            // one-time price for OXXO annual
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
	if cfg.PriceStandardAnnual != "" {
		prices[gymDomain.PlanStandardAnnual] = cfg.PriceStandardAnnual
	}
	if cfg.PricePlus != "" {
		prices[gymDomain.PlanPlusMonthly] = cfg.PricePlus
	}
	return &StripeGateway{
		client:            stripe.NewClient(cfg.SecretKey),
		prices:            prices,
		oxxoAnnualPriceID: cfg.PriceStandardAnnualOXXO,
	}
}

func (g *StripeGateway) Provider() subDomain.Provider { return subDomain.ProviderStripe }

// StartCheckout creates a Stripe Checkout Session and returns its hosted URL.
// Default mode=subscription (card → Stripe Subscription); branch to
// mode=payment one-time when PaymentMethod=="oxxo" (Stripe limitation:
// OXXO no convive con Subscriptions).
//
// In both branches we pin the gym id into both `metadata.gym_id` and
// `client_reference_id` so the webhook side (which already inspects both,
// see parseStripeEvent) can resolve the gym regardless of which event arrives
// first.
func (g *StripeGateway) StartCheckout(ctx context.Context, in subDomain.CheckoutRequest) (subDomain.CheckoutResult, error) {
	if in.PaymentMethod == "oxxo" {
		return g.startCheckoutOXXO(ctx, in)
	}
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
		// Subscription created by the session inherits this metadata, so
		// `customer.subscription.*` webhooks carry gym_id in object.metadata.
		// OJO: las invoices de renovación NO la exponen en invoice.metadata —
		// Stripe la copia en parent.subscription_details.metadata (Basil+) /
		// subscription_details.metadata (legacy); el parser la lee de ahí
		// (extractStripeInvoiceGymID).
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

// startCheckoutOXXO crea una sesión Checkout en mode=payment con OXXO como
// único método de pago. Sólo aplica al plan anual (validado upstream en el
// use case). Cuando el cliente paga la ficha en tienda, Stripe emite
// payment_intent.succeeded que el webhook parser mapea a EventActivated.
// El metadata viaja también vía payment_intent_data para que el PI lo lleve
// (la metadata del Checkout Session no se propaga sola al PaymentIntent).
//
// expires_after_days: 3 (default de Stripe en MX). Cubre la ventana realista
// de "voy mañana al OXXO" sin dejar fichas eternamente abiertas. Stripe deja
// configurar hasta 7; 3 es suficiente y reduce la incertidumbre del dueño.
func (g *StripeGateway) startCheckoutOXXO(ctx context.Context, in subDomain.CheckoutRequest) (subDomain.CheckoutResult, error) {
	if g.oxxoAnnualPriceID == "" {
		return subDomain.CheckoutResult{}, fmt.Errorf("stripe: %w (oxxo annual price not configured)", subDomain.ErrUnsupportedPlan)
	}
	gymRef := in.GymID.String()
	piMetadata := map[string]string{
		"gym_id":         gymRef,
		"plan":           in.Plan,
		"payment_method": "oxxo",
	}
	params := &stripe.CheckoutSessionCreateParams{
		Mode:               stripe.String("payment"),
		SuccessURL:         stripe.String(in.SuccessURL),
		CancelURL:          stripe.String(in.CancelURL),
		ClientReferenceID:  stripe.String(gymRef),
		Locale:             stripe.String("es-419"),
		PaymentMethodTypes: stripe.StringSlice([]string{"oxxo"}),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{
				Price:    stripe.String(g.oxxoAnnualPriceID),
				Quantity: stripe.Int64(1),
			},
		},
		PaymentIntentData: &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			Metadata: piMetadata,
		},
		Metadata: map[string]string{
			"gym_id":         gymRef,
			"plan":           in.Plan,
			"payment_method": "oxxo",
		},
	}
	if in.CustomerEmail != "" {
		params.CustomerEmail = stripe.String(in.CustomerEmail)
	}

	sess, err := g.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return subDomain.CheckoutResult{}, fmt.Errorf("stripe: create oxxo checkout session: %w", err)
	}
	if sess == nil || sess.URL == "" {
		return subDomain.CheckoutResult{}, errors.New("stripe: empty oxxo session url")
	}
	return subDomain.CheckoutResult{URL: sess.URL, SessionID: sess.ID}, nil
}

// StartBillingPortal opens a Stripe-hosted Customer Portal session for the
// given customer id. The portal lets the owner update card, see invoices,
// change plan, or cancel without us touching state — Stripe pushes the
// resulting events back to the webhook.
func (g *StripeGateway) StartBillingPortal(ctx context.Context, in subDomain.BillingPortalRequest) (subDomain.BillingPortalResult, error) {
	if in.CustomerID == "" {
		return subDomain.BillingPortalResult{}, errors.New("stripe: customer_id required for portal session")
	}
	params := &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(in.CustomerID),
		ReturnURL: stripe.String(in.ReturnURL),
	}
	sess, err := g.client.V1BillingPortalSessions.Create(ctx, params)
	if err != nil {
		return subDomain.BillingPortalResult{}, fmt.Errorf("stripe: create billing portal session: %w", err)
	}
	if sess == nil || sess.URL == "" {
		return subDomain.BillingPortalResult{}, errors.New("stripe: empty portal session url")
	}
	return subDomain.BillingPortalResult{URL: sess.URL}, nil
}
