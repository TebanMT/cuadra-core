// Package controllers exposes the subscription HTTP surface.
//
// Two flavors of routes:
//  1. /api/v1/subscriptions/me     — authenticated, owner-only, gym reads its
//     own subscription state + recent events.
//  2. /api/v1/webhooks/{stripe,mercadopago}
//     — public, signature-verified by helper,
//     processor pushes events here.
//
// Webhook signature verification is intentionally a small set of helpers (see
// `helpers.go`) so we can swap in real Stripe/MP SDKs once we have keys
// without changing the controller wiring.
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

type SubscriptionController struct {
	Record   *subApp.RecordEvent
	Get      *subApp.GetSubscription
	Checkout *subApp.StartCheckout
	// Portal — optional. Wired in cloud (cmd/server). In sidecar build, the
	// controller exists only to expose GET /subscriptions/me; opening the
	// billing portal requires Stripe and lives cloud-only.
	Portal   *subApp.StartBillingPortal
	Verifier *WebhookVerifier
	Tokens   auth.TokenService
}

func NewSubscriptionController(record *subApp.RecordEvent, get *subApp.GetSubscription, checkout *subApp.StartCheckout, portal *subApp.StartBillingPortal, verifier *WebhookVerifier, tokens auth.TokenService) *SubscriptionController {
	return &SubscriptionController{Record: record, Get: get, Checkout: checkout, Portal: portal, Verifier: verifier, Tokens: tokens}
}

// RegisterRoutes binds both the authenticated /subscriptions/* and the
// public /webhooks/* endpoints. The webhook routes intentionally live OUTSIDE
// the AuthMiddleware group; signature verification is the auth.
func (ctrl *SubscriptionController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	authed := api.Group("/subscriptions")
	authed.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		authed.GET("/me", middleware.RequireOwner(), ctrl.handleGetMine)
		authed.POST("/me/extend-trial", middleware.RequireOwner(), ctrl.handleExtendTrial)
		authed.POST("/checkout-session", middleware.RequireOwner(), ctrl.handleStartCheckout)
		authed.POST("/billing-portal-session", middleware.RequireOwner(), ctrl.handleStartBillingPortal)
	}
	wh := r.Group("/api/v1/webhooks")
	wh.POST("/stripe", ctrl.handleStripe)
	wh.POST("/mercadopago", ctrl.handleMercadoPago)
}

// RegisterReadOnlyRoutes expone solamente GET /subscriptions/me. El sidecar
// lo monta porque la página de Suscripción del desktop necesita poder leer el
// plan/status para no caer en error de carga, pero el resto del surface
// (checkout, webhooks, extend-trial) son cloud-only — el dueño hace ese flujo
// desde el browser cuando hay internet. La historia de eventos puede venir
// vacía si el repo del sidecar es un stub (no sincronizamos
// `subscription_events` todavía); el FE renderea "aún no hay movimientos"
// como empty state.
func (ctrl *SubscriptionController) RegisterReadOnlyRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	authed := api.Group("/subscriptions")
	authed.Use(middleware.AuthMiddleware(ctrl.Tokens))
	authed.GET("/me", middleware.RequireOwner(), ctrl.handleGetMine)
}

// ---------------------------------------------------------------------------
// Read.
// ---------------------------------------------------------------------------

type subscriptionWire struct {
	Plan           string             `json:"plan"`
	Status         string             `json:"status"`
	TrialEndsAt    *time.Time         `json:"trial_ends_at,omitempty"`
	PeriodEndsAt   *time.Time         `json:"period_ends_at,omitempty"`
	HasActiveAcc   bool               `json:"has_active_access"`
	IsTrialExpired bool               `json:"is_trial_expired"`
	History        []subscriptionEvWi `json:"history"`
}

type subscriptionEvWi struct {
	ID         uuid.UUID  `json:"id"`
	Provider   string     `json:"provider"`
	Type       string     `json:"type"`
	Plan       string     `json:"plan"`
	Amount     *float64   `json:"amount,omitempty"`
	Currency   *string    `json:"currency,omitempty"`
	OccurredAt time.Time  `json:"occurred_at"`
	PeriodEnd  *time.Time `json:"period_ends_at,omitempty"`
	// VoucherURL — sólo presente en eventos type=voucher_emitted (OXXO).
	// Es el link al PDF de la ficha hosteado por Stripe. El dashboard lo
	// usa para que el dueño re-abra la ficha si perdió el email.
	VoucherURL string `json:"voucher_url,omitempty"`
}

func (ctrl *SubscriptionController) handleGetMine(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errors.New("missing gym in context"))
		return
	}
	out, err := ctrl.Get.Execute(c.Request.Context(), gymID)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	hist := make([]subscriptionEvWi, 0, len(out.History))
	for _, e := range out.History {
		voucherURL := ""
		if e.Type == subDomain.EventVoucherEmitted && e.RawPayload != nil {
			if v, ok := e.RawPayload["voucher_url"].(string); ok {
				voucherURL = v
			}
		}
		hist = append(hist, subscriptionEvWi{
			ID:         e.ID,
			Provider:   string(e.Provider),
			Type:       string(e.Type),
			Plan:       e.Plan,
			Amount:     e.Amount,
			Currency:   e.Currency,
			OccurredAt: e.OccurredAt,
			PeriodEnd:  e.PeriodEndsAt,
			VoucherURL: voucherURL,
		})
	}
	utils.JsonResponse(c, http.StatusOK, subscriptionWire{
		Plan:           out.Plan,
		Status:         out.Status,
		TrialEndsAt:    out.TrialEndsAt,
		PeriodEndsAt:   out.PeriodEndsAt,
		HasActiveAcc:   out.HasActiveAcc,
		IsTrialExpired: out.IsTrialExpired,
		History:        hist,
	})
}

// ---------------------------------------------------------------------------
// Start checkout (Stripe Checkout Session / MP Preapproval).
// ---------------------------------------------------------------------------

type startCheckoutReq struct {
	Provider      string `json:"provider"`       // "stripe" | "mercadopago"
	Plan          string `json:"plan"`           // "standard_monthly" | "standard_annual" | "plus_monthly" | "plus_annual"
	PaymentMethod string `json:"payment_method"` // "" | "card" | "oxxo" (oxxo solo Stripe + standard_annual)
}

type startCheckoutResp struct {
	URL       string `json:"url"`
	SessionID string `json:"session_id,omitempty"`
}

// handleStartCheckout creates a hosted checkout URL for the requested
// provider and returns it for the FE to redirect into the system browser.
// The FE never sees the secret key — Stripe / MP authenticate the call
// server-side. The webhook stays the source of truth for state changes.
func (ctrl *SubscriptionController) handleStartCheckout(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errors.New("missing gym in context"))
		return
	}
	userID, ok := middleware.GetUserID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errors.New("missing user in context"))
		return
	}
	var req startCheckoutReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	plan := strings.TrimSpace(req.Plan)
	method := strings.ToLower(strings.TrimSpace(req.PaymentMethod))
	out, err := ctrl.Checkout.Execute(c.Request.Context(), subApp.StartCheckoutInput{
		GymID:         gymID,
		UserID:        userID,
		Provider:      subDomain.Provider(provider),
		Plan:          plan,
		PaymentMethod: method,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, startCheckoutResp{URL: out.URL, SessionID: out.SessionID})
}

// ---------------------------------------------------------------------------
// Billing portal (Stripe-hosted self-service).
// ---------------------------------------------------------------------------

type billingPortalResp struct {
	URL string `json:"url"`
}

// handleStartBillingPortal abre una sesión de Stripe Customer Portal para que
// el dueño actualice tarjeta, vea facturas, cambie de plan o cancele sin
// volver a entrar el método de pago. Stripe maneja toda la UI; nosotros
// recibimos los efectos por webhook.
//
// Si el gym aún no ha pasado por un primer checkout (no hay
// `gym.stripe_customer_id` persistido), devolvemos 409 con un copy claro —
// no hay nada que mostrar en el portal hasta que la primera mensualidad se
// haya cobrado.
func (ctrl *SubscriptionController) handleStartBillingPortal(c *gin.Context) {
	if ctrl.Portal == nil {
		utils.ErrorResponse(c, http.StatusServiceUnavailable, errors.New("billing portal not configured"))
		return
	}
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errors.New("missing gym in context"))
		return
	}
	out, err := ctrl.Portal.Execute(c.Request.Context(), subApp.StartBillingPortalInput{GymID: gymID})
	if err != nil {
		// Map ErrNoStripeCustomer specifically to 409 — the FE shows a
		// distinct toast ("activa tu plan primero") vs the generic 5xx.
		if errors.Is(err, subApp.ErrNoStripeCustomer) {
			utils.ErrorResponse(c, http.StatusConflict, err)
			return
		}
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, billingPortalResp{URL: out.URL})
}

// ---------------------------------------------------------------------------
// Manual extend-trial (sales tool).
// ---------------------------------------------------------------------------

type extendTrialReq struct {
	Days int `json:"days"`
}

func (ctrl *SubscriptionController) handleExtendTrial(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	var req extendTrialReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	if req.Days <= 0 || req.Days > 90 {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("days must be 1..90"))
		return
	}
	out, err := ctrl.Record.Execute(c.Request.Context(), subApp.RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderManual,
		Type:       subDomain.EventTrialExtended,
		ExternalID: "manual-" + uuid.NewString(),
		Plan:       "",
		RawPayload: map[string]any{"days": float64(req.Days)},
		OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"applied": out.Applied, "event_id": out.EventID})
}

// ---------------------------------------------------------------------------
// Stripe webhook.
// ---------------------------------------------------------------------------
// Real verification will swap to github.com/stripe/stripe-go/webhook.ConstructEvent
// once the founder configures STRIPE_WEBHOOK_SECRET. Until then the helper
// accepts the body if STRIPE_WEBHOOK_SECRET is empty (development) and rejects
// otherwise.
func (ctrl *SubscriptionController) handleStripe(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	sig := c.GetHeader("Stripe-Signature")
	if err := ctrl.Verifier.VerifyStripe(body, sig); err != nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, err)
		return
	}
	in, err := parseStripeEvent(body)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	if in == nil {
		// Event we don't act on — return 200 so Stripe doesn't retry.
		c.Status(http.StatusOK)
		return
	}
	out, err := ctrl.Record.Execute(c.Request.Context(), *in)
	if err != nil {
		// Log diagnóstico antes de devolver el error. La causa más común de
		// "aún faltan datos de configuración" es que el price en Stripe tiene
		// un nickname distinto de los SKUs canónicos (standard_monthly /
		// standard_annual / plus_monthly / plus_annual). Con este log el
		// dueño ve exactamente qué plan se extrajo del webhook y puede
		// corregir el nickname en el dashboard de Stripe.
		log.Printf("[subscriptions] stripe webhook record failed: external_id=%s type=%s plan=%q gym_id=%s err=%v",
			in.ExternalID, in.Type, in.Plan, in.GymID, err)
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"applied": out.Applied})
}

// ---------------------------------------------------------------------------
// Mercado Pago webhook.
// ---------------------------------------------------------------------------
// MP signs with `x-signature` (HMAC SHA-256) + `x-request-id`. Same shape:
// verify, parse, hand to Record.
func (ctrl *SubscriptionController) handleMercadoPago(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	sig := c.GetHeader("X-Signature")
	rid := c.GetHeader("X-Request-Id")
	if err := ctrl.Verifier.VerifyMercadoPago(body, sig, rid, c.Query("data.id")); err != nil {
		utils.ErrorResponse(c, http.StatusUnauthorized, err)
		return
	}
	in, err := parseMercadoPagoEvent(body)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	if in == nil {
		c.Status(http.StatusOK)
		return
	}
	out, err := ctrl.Record.Execute(c.Request.Context(), *in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"applied": out.Applied})
}

// ---------------------------------------------------------------------------
// Stripe / MP payload mappers.
// ---------------------------------------------------------------------------
// We accept the raw event JSON and normalise just the fields RecordEvent
// needs. Full Stripe SDK integration is deferred until the founder configures
// keys — at which point this gets replaced with a typed `event.Data.Object`.

type stripeEnvelope struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Created int64          `json:"created"`
	Data    map[string]any `json:"data"`
}

func parseStripeEvent(body []byte) (*subApp.RecordEventInput, error) {
	var env stripeEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	gymID, plan, periodEnd, amount, currency := extractStripeFields(env.Data)
	customerID := extractStripeCustomer(env.Data)
	if strings.HasPrefix(env.Type, "invoice.") {
		// El objeto invoice NO hereda la metadata de la subscription en su
		// propio `metadata` (llega vacía), y tampoco trae current_period_end
		// ni `items` — extractStripeFields está escrito para la forma del
		// subscription object. Suplimos con la forma real de la invoice.
		// Bug jul-2026: sin esto TODA renovación mensual caía al drop de
		// gym_id irresoluble → 200 OK a Stripe sin registrar nada.
		if gymID == uuid.Nil {
			gymID = extractStripeInvoiceGymID(env.Data)
		}
		if periodEnd == nil {
			periodEnd = extractStripeInvoicePeriodEnd(env.Data)
		}
		// "" cuando la línea no trae nickname (API Basil+): RecordEvent cae
		// al plan vigente del gym — correcto para una renovación, pagar no
		// cambia de plan. El default "standard_monthly" de extractStripeFields
		// aquí sería un downgrade silencioso para gyms Plus.
		plan = extractStripeInvoicePlan(env.Data)
	}
	if gymID == uuid.Nil {
		// We need to know which gym this concerns. Stripe's `client_reference_id`
		// or `metadata.gym_id` carries it in our setup — if missing, drop the
		// event (returning nil bypasses RecordEvent). Este drop responde 200
		// (Stripe no reintenta); el log es el único rastro que queda.
		log.Printf("[subscriptions] stripe webhook dropped, unresolvable gym_id: type=%s id=%s", env.Type, env.ID)
		return nil, nil
	}
	occurred := time.Unix(env.Created, 0).UTC()
	if env.Created == 0 {
		occurred = time.Now().UTC()
	}
	var typ subDomain.EventType
	externalID := env.ID
	rawPayload := map[string]any{"event": env.Type, "data": env.Data}
	switch env.Type {
	case "customer.subscription.created":
		typ = subDomain.EventActivated
	case "invoice.paid", "invoice.payment_succeeded":
		// Ambos eventos disparan por el MISMO cobro (payment_succeeded = el
		// intento de pago funcionó, paid = la invoice quedó saldada). Para no
		// duplicar filas en la historia:
		//   - el primer cobro del checkout (billing_reason=subscription_create)
		//     se ignora — customer.subscription.created ya registra la
		//     activación con plan + period end;
		//   - external id = la invoice (in_...), no el event (evt_...): la
		//     idempotencia por (provider, external_id) colapsa paid +
		//     payment_succeeded del mismo cobro en UNA sola fila.
		if extractStripeInvoiceBillingReason(env.Data) == "subscription_create" {
			return nil, nil
		}
		typ = subDomain.EventRenewed
		if invID := extractStripeObjectID(env.Data); invID != "" {
			externalID = invID
		}
	case "invoice.payment_failed":
		// Conserva el evt_... como external id: cada reintento de dunning
		// fallido es un evento distinto que sí queremos registrar, y el
		// in_... queda libre para que el invoice.paid de un cobro tardío no
		// choque contra el fallo previo en el índice de idempotencia.
		typ = subDomain.EventPastDue
	case "customer.subscription.deleted":
		typ = subDomain.EventCancelled
	case "customer.subscription.updated":
		// El dueño tocó algo en el Customer Portal de Stripe. Nos interesan
		// dos transiciones:
		//   - cancel_at_period_end = true  → cancel programado. Bajamos a
		//     status=cancelled inmediatamente; el grace period vive en
		//     subscription_ends_at (que ya tenía la fecha del periodo actual),
		//     así que HasActiveAccess sigue true hasta esa fecha.
		//   - cancel_at_period_end = false (después de haber sido true) →
		//     el dueño se arrepintió y reactivó. Re-emite EventActivated
		//     para volver a status=active.
		// Cualquier otro update (cambio de tarjeta, etc.) no nos toca —
		// devolvemos nil para que Stripe reciba 200 sin RecordEvent.
		switch detectStripeCancelTransition(env.Data) {
		case "cancel_scheduled":
			typ = subDomain.EventCancelled
		case "reactivated":
			typ = subDomain.EventActivated
		default:
			return nil, nil
		}

	// --- OXXO (mode=payment one-time, sólo plan anual) -------------------
	// Stripe NO permite OXXO en mode=subscription, así que el flow no pasa
	// por customer.subscription.* / invoice.*. Lo modelamos como:
	//   checkout.session.completed (payment_status=unpaid) → voucher_emitted
	//   payment_intent.succeeded                            → activated
	//   payment_intent.payment_failed                       → voucher_expired
	case "checkout.session.completed":
		mode, paymentStatus, _ := extractStripeCheckoutDetails(env.Data)
		if mode != "payment" {
			// El path subscription (default) ya lo cubre customer.subscription.created.
			return nil, nil
		}
		// OXXO con mode=payment. Si el cliente pagó tarjeta inline durante
		// el Checkout (poco común pero posible), el status llega "paid" y
		// activamos directo; si dejó la ficha pendiente, status="unpaid"
		// y registramos el voucher.
		if paymentStatus == "paid" {
			typ = subDomain.EventActivated
			plan = inferPlanForOXXO(env.Data, plan)
			periodEnd = oneYearFrom(occurred)
		} else {
			typ = subDomain.EventVoucherEmitted
			plan = inferPlanForOXXO(env.Data, plan)
			if exp := extractOXXOVoucherExpiry(env.Data); exp != nil {
				periodEnd = exp
			}
			if url := extractOXXOVoucherURL(env.Data); url != "" {
				rawPayload["voucher_url"] = url
			}
		}
		// Marker top-level del método de pago. Se planta en TODAS las ramas
		// OXXO (activated, voucher_emitted, voucher_expired) para que el cron
		// de renovación pueda filtrar candidatos con un query JSONB simple
		// (raw_payload->>'payment_method' = 'oxxo') en lugar de descender por
		// data.object.metadata. Idempotente: si ya estaba (caso edge), no
		// rompe.
		rawPayload["payment_method"] = "oxxo"
	case "payment_intent.succeeded":
		// Sólo nos interesa si trae metadata.gym_id — ese es el marker que
		// pusimos en startCheckoutOXXO. Otros payment intents (futuros
		// flows tipo tap-to-sell) los ignoramos.
		if !isOXXOPaymentIntent(env.Data) {
			return nil, nil
		}
		typ = subDomain.EventActivated
		plan = inferPlanForOXXO(env.Data, plan)
		periodEnd = oneYearFrom(occurred)
		rawPayload["payment_method"] = "oxxo"
	case "payment_intent.payment_failed":
		if !isOXXOPaymentIntent(env.Data) {
			return nil, nil
		}
		typ = subDomain.EventVoucherExpired
		plan = inferPlanForOXXO(env.Data, plan)
		rawPayload["payment_method"] = "oxxo"
	default:
		return nil, nil
	}
	return &subApp.RecordEventInput{
		GymID:            gymID,
		Provider:         subDomain.ProviderStripe,
		Type:             typ,
		ExternalID:       externalID,
		Plan:             plan,
		Amount:           amount,
		Currency:         currency,
		PeriodEndsAt:     periodEnd,
		StripeCustomerID: customerID,
		RawPayload:       rawPayload,
		OccurredAt:       occurred,
	}, nil
}

// detectStripeCancelTransition mira el payload de customer.subscription.updated
// y decide si representa una transición que nos importa.
//
// Stripe incluye `previous_attributes` con los campos que cambiaron — esa es
// la fuente de verdad para detectar "transición". Si cancel_at_period_end
// aparece ahí, el dueño acabó de cambiar el flag (ya sea cancelando o
// reactivando). Combinado con el VALOR ACTUAL en object.cancel_at_period_end
// nos dice qué pasó:
//   - actual=true  + previous.cancel_at_period_end existe (era false) → cancel_scheduled
//   - actual=false + previous.cancel_at_period_end existe (era true)  → reactivated
//
// Cualquier otro update (cambio de tarjeta, prórroga de trial, etc.) cae a
// "" y el caller devuelve nil para que Stripe reciba 200 sin RecordEvent.
func detectStripeCancelTransition(data map[string]any) string {
	if data == nil {
		return ""
	}
	prev, _ := data["previous_attributes"].(map[string]any)
	if prev == nil {
		return ""
	}
	if _, changed := prev["cancel_at_period_end"]; !changed {
		return ""
	}
	obj, _ := data["object"].(map[string]any)
	if obj == nil {
		return ""
	}
	current, _ := obj["cancel_at_period_end"].(bool)
	if current {
		return "cancel_scheduled"
	}
	return "reactivated"
}

// extractStripeCheckoutDetails saca `mode` y `payment_status` del payload
// de checkout.session.completed. Devuelve "" si faltan.
func extractStripeCheckoutDetails(data map[string]any) (mode, paymentStatus, paymentMethod string) {
	if data == nil {
		return "", "", ""
	}
	obj, _ := data["object"].(map[string]any)
	if obj == nil {
		return "", "", ""
	}
	if v, ok := obj["mode"].(string); ok {
		mode = v
	}
	if v, ok := obj["payment_status"].(string); ok {
		paymentStatus = v
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		if v, ok := md["payment_method"].(string); ok {
			paymentMethod = v
		}
	}
	return mode, paymentStatus, paymentMethod
}

// isOXXOPaymentIntent reporta si el payment_intent.* trae metadata.gym_id +
// metadata.payment_method=oxxo. Esos markers los plantamos nosotros en
// startCheckoutOXXO; otros PIs no llevan ese par.
func isOXXOPaymentIntent(data map[string]any) bool {
	if data == nil {
		return false
	}
	obj, _ := data["object"].(map[string]any)
	if obj == nil {
		return false
	}
	md, _ := obj["metadata"].(map[string]any)
	if md == nil {
		return false
	}
	if _, ok := md["gym_id"].(string); !ok {
		return false
	}
	if v, ok := md["payment_method"].(string); ok && v == "oxxo" {
		return true
	}
	return false
}

// inferPlanForOXXO toma el plan del metadata si vino, o cae a
// gymDomain.PlanStandardAnnual (hoy el único plan elegible para OXXO; la
// validación upstream en StartCheckout lo garantiza). El fallback existe
// para webhooks legacy o re-entregas viejas.
func inferPlanForOXXO(data map[string]any, parsed string) string {
	if data != nil {
		if obj, ok := data["object"].(map[string]any); ok {
			if md, ok := obj["metadata"].(map[string]any); ok {
				if v, ok := md["plan"].(string); ok && v != "" {
					return v
				}
			}
		}
	}
	if parsed != "" && parsed != "standard_monthly" {
		return parsed
	}
	return "standard_annual"
}

// extractOXXOVoucherExpiry intenta leer el momento en que vence el voucher
// desde el payload del checkout.session.completed. Stripe lo coloca en
// payment_intent.next_action.oxxo_display_details.expires_after (epoch
// seconds). Si no aparece, devolvemos nil — el dashboard cae a un mensaje
// genérico "dentro de 3 días" y Stripe email manda la fecha real.
func extractOXXOVoucherExpiry(data map[string]any) *time.Time {
	if data == nil {
		return nil
	}
	obj, _ := data["object"].(map[string]any)
	if obj == nil {
		return nil
	}
	pi, _ := obj["payment_intent"].(map[string]any)
	if pi == nil {
		return nil
	}
	na, _ := pi["next_action"].(map[string]any)
	if na == nil {
		return nil
	}
	od, _ := na["oxxo_display_details"].(map[string]any)
	if od == nil {
		return nil
	}
	switch v := od["expires_after"].(type) {
	case float64:
		t := time.Unix(int64(v), 0).UTC()
		return &t
	case int64:
		t := time.Unix(v, 0).UTC()
		return &t
	}
	return nil
}

// extractOXXOVoucherURL saca el link al PDF de la ficha hosteado por Stripe
// (payment_intent.next_action.oxxo_display_details.hosted_voucher_url). Lo
// guardamos en RawPayload para que el dashboard pueda re-mostrarlo si el
// dueño cierra el email de Stripe. Devuelve "" si no viene.
func extractOXXOVoucherURL(data map[string]any) string {
	if data == nil {
		return ""
	}
	obj, _ := data["object"].(map[string]any)
	if obj == nil {
		return ""
	}
	pi, _ := obj["payment_intent"].(map[string]any)
	if pi == nil {
		return ""
	}
	na, _ := pi["next_action"].(map[string]any)
	if na == nil {
		return ""
	}
	od, _ := na["oxxo_display_details"].(map[string]any)
	if od == nil {
		return ""
	}
	if v, ok := od["hosted_voucher_url"].(string); ok {
		return v
	}
	return ""
}

// oneYearFrom devuelve t + 1 año en UTC, para usar como PeriodEndsAt del
// plan anual activado vía OXXO. No usamos current_period_end de Stripe
// porque NO hay Stripe Subscription en este flow (mode=payment one-time).
func oneYearFrom(t time.Time) *time.Time {
	end := t.AddDate(1, 0, 0).UTC()
	return &end
}

// ---------------------------------------------------------------------------
// Invoice-shaped events (invoice.paid / invoice.payment_succeeded / _failed).
// ---------------------------------------------------------------------------
// La invoice tiene su propia forma, distinta del subscription object que
// asume extractStripeFields; estos helpers leen los campos donde la invoice
// realmente los trae.

// digMap desciende por claves anidadas de mapas JSON genéricos. Devuelve nil
// si cualquier eslabón falta o no es objeto (leer de un map nil es seguro).
func digMap(m map[string]any, keys ...string) map[string]any {
	cur := m
	for _, k := range keys {
		next, _ := cur[k].(map[string]any)
		cur = next
	}
	return cur
}

// extractStripeInvoiceGymID resuelve el gym en eventos invoice.*. Stripe copia
// la metadata de la subscription (donde StartCheckout siembra gym_id vía
// SubscriptionData.Metadata) en:
//   - parent.subscription_details.metadata — API 2025-03-31 (Basil) en
//     adelante, incluida 2026-04-22.dahlia; o
//   - subscription_details.metadata — API 2023-08-16 .. 2025-02 (legacy).
//
// Probamos ambas, espejo del patrón dual de current_period_end en
// extractStripeFields.
func extractStripeInvoiceGymID(data map[string]any) uuid.UUID {
	obj := digMap(data, "object")
	for _, md := range []map[string]any{
		digMap(obj, "parent", "subscription_details", "metadata"),
		digMap(obj, "subscription_details", "metadata"),
	} {
		if g, ok := md["gym_id"].(string); ok {
			if id, err := uuid.Parse(g); err == nil {
				return id
			}
		}
	}
	return uuid.Nil
}

// firstStripeInvoiceLine devuelve lines.data[0] del objeto invoice, o nil.
// Tomar la primera línea basta para nuestro caso: cada subscription tiene un
// único price (un plan por gym).
func firstStripeInvoiceLine(data map[string]any) map[string]any {
	ld, _ := digMap(data, "object", "lines")["data"].([]any)
	if len(ld) == 0 {
		return nil
	}
	first, _ := ld[0].(map[string]any)
	return first
}

// extractStripeInvoicePeriodEnd lee el fin del periodo que este cobro cubre
// desde lines.data[0].period.end (epoch seconds). La invoice no trae
// current_period_end — ese campo vive en el subscription object.
func extractStripeInvoicePeriodEnd(data map[string]any) *time.Time {
	if v, ok := digMap(firstStripeInvoiceLine(data), "period")["end"].(float64); ok && v > 0 {
		t := time.Unix(int64(v), 0).UTC()
		return &t
	}
	return nil
}

// extractStripeInvoicePlan intenta el nickname del price en la primera línea
// (line.plan.nickname o line.price.nickname, presentes en API legacy). En
// Basil+ la línea trae `pricing` sin nickname → devolvemos "" y RecordEvent
// conserva el plan actual del gym.
func extractStripeInvoicePlan(data map[string]any) string {
	line := firstStripeInvoiceLine(data)
	for _, key := range []string{"plan", "price"} {
		if nick, ok := digMap(line, key)["nickname"].(string); ok && nick != "" {
			return nick
		}
	}
	return ""
}

// extractStripeInvoiceBillingReason devuelve object.billing_reason
// ("subscription_create" | "subscription_cycle" | "subscription_update" | …)
// o "" si falta.
func extractStripeInvoiceBillingReason(data map[string]any) string {
	if v, ok := digMap(data, "object")["billing_reason"].(string); ok {
		return v
	}
	return ""
}

// extractStripeObjectID devuelve object.id (p.ej. la invoice "in_...") o "".
func extractStripeObjectID(data map[string]any) string {
	if v, ok := digMap(data, "object")["id"].(string); ok {
		return v
	}
	return ""
}

// extractStripeCustomer pulls the customer id out of whichever event shape
// Stripe sent. customer.subscription.* y invoice.* lo traen como string
// directo en object.customer; checkout.session.completed lo trae expandido
// como object. Devuelve "" si no hay (e.g. trial events sin customer aún).
func extractStripeCustomer(data map[string]any) string {
	if data == nil {
		return ""
	}
	obj, _ := data["object"].(map[string]any)
	if obj == nil {
		return ""
	}
	if s, ok := obj["customer"].(string); ok && s != "" {
		return s
	}
	if c, ok := obj["customer"].(map[string]any); ok {
		if s, ok := c["id"].(string); ok {
			return s
		}
	}
	return ""
}

// extractStripeFields pulls out the subset of the Stripe event payload we
// need. The shape varies per event type so we're forgiving — missing pieces
// resolve to zero values.
func extractStripeFields(data map[string]any) (uuid.UUID, string, *time.Time, *float64, *string) {
	if data == nil {
		return uuid.Nil, "", nil, nil, nil
	}
	obj, _ := data["object"].(map[string]any)
	if obj == nil {
		return uuid.Nil, "", nil, nil, nil
	}
	var gymID uuid.UUID
	if md, ok := obj["metadata"].(map[string]any); ok {
		if g, ok := md["gym_id"].(string); ok {
			if id, err := uuid.Parse(g); err == nil {
				gymID = id
			}
		}
	}
	if gymID == uuid.Nil {
		if cr, ok := obj["client_reference_id"].(string); ok {
			if id, err := uuid.Parse(cr); err == nil {
				gymID = id
			}
		}
	}
	plan := ""
	// firstItem captura el primer subscription item — tanto para extraer el
	// plan.nickname como para leer current_period_end en la API moderna
	// (2026-04-22.dahlia+) donde Stripe movió ese campo del subscription
	// object a items.data[0].current_period_end.
	var firstItem map[string]any
	if items, ok := obj["items"].(map[string]any); ok {
		if datas, ok := items["data"].([]any); ok && len(datas) > 0 {
			if first, ok := datas[0].(map[string]any); ok {
				firstItem = first
				if p, ok := first["plan"].(map[string]any); ok {
					if nick, ok := p["nickname"].(string); ok {
						plan = nick
					}
				}
			}
		}
	}
	if plan == "" {
		// Fallback: Standard mensual es el SKU por defecto cuando Stripe no nos
		// devuelve nickname (gym sin price asignado en dashboard).
		plan = "standard_monthly"
	}
	// current_period_end: legacy en root del subscription (API <=2025-08-27);
	// moderna en items.data[0].current_period_end (API 2025-09 en adelante,
	// incluida 2026-04-22.dahlia). Probamos ambos para soportar webhooks de
	// cualquier API version sin requerir pinear el endpoint en el dashboard.
	var periodEnd *time.Time
	if pe, ok := obj["current_period_end"].(float64); ok && pe > 0 {
		t := time.Unix(int64(pe), 0).UTC()
		periodEnd = &t
	} else if firstItem != nil {
		if pe, ok := firstItem["current_period_end"].(float64); ok && pe > 0 {
			t := time.Unix(int64(pe), 0).UTC()
			periodEnd = &t
		}
	}
	var amount *float64
	if v, ok := obj["amount_paid"].(float64); ok {
		val := v / 100.0
		amount = &val
	} else if v, ok := obj["amount"].(float64); ok {
		val := v / 100.0
		amount = &val
	}
	var currency *string
	if c, ok := obj["currency"].(string); ok {
		v := strings.ToUpper(c)
		currency = &v
	}
	return gymID, plan, periodEnd, amount, currency
}

// ---------------------------------------------------------------------------
// Mercado Pago.
// ---------------------------------------------------------------------------

type mpEnvelope struct {
	ID         int64                  `json:"id"`
	LiveMode   bool                   `json:"live_mode"`
	Type       string                 `json:"type"`
	Action     string                 `json:"action"`
	DateCreate string                 `json:"date_created"`
	Data       map[string]interface{} `json:"data"`
}

func parseMercadoPagoEvent(body []byte) (*subApp.RecordEventInput, error) {
	var env mpEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	// `subscription_authorized_payment` es el evento que MP emite en cada
	// cobro recurrente de un preapproval (los meses 2+, tras `payment` del
	// primer cargo). Sin él el gym entra a `active` con el primer pago pero
	// nunca renueva su `subscription_ends_at` y termina en `past_due` por
	// el gate. Ver review de auditoría blockers #2.
	if env.Type != "subscription_preapproval" &&
		env.Type != "subscription_authorized_payment" &&
		env.Type != "payment" {
		return nil, nil
	}
	// MP's webhook is a notification — we'd normally fetch the resource by id.
	// The minimum fields we need are passed via metadata or external_reference.
	gymID := uuid.Nil
	if ref, ok := env.Data["external_reference"].(string); ok {
		if id, err := uuid.Parse(ref); err == nil {
			gymID = id
		}
	}
	if gymID == uuid.Nil {
		return nil, nil
	}
	var typ subDomain.EventType
	switch env.Action {
	case "payment.created", "payment.updated":
		// Primer cobro de la preapproval o re-procesamiento de un cargo
		// individual. Lo tratamos como activation/renewal y dejamos que la
		// idempotencia por (provider, external_id) deduplique.
		typ = subDomain.EventActivated
	case "subscription_authorized_payment.created",
		"subscription_authorized_payment.updated":
		// Cobro recurrente de un preapproval activo — mes 2+. Distinto a
		// `payment` para que el state machine sepa que NO es la activación
		// inicial sino una renovación que extiende `subscription_ends_at`.
		typ = subDomain.EventRenewed
	case "subscription_preapproval.cancelled":
		typ = subDomain.EventCancelled
	default:
		return nil, nil
	}
	occurred := time.Now().UTC()
	if env.DateCreate != "" {
		if t, err := time.Parse(time.RFC3339, env.DateCreate); err == nil {
			occurred = t.UTC()
		}
	}
	plan := "standard_monthly"
	return &subApp.RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderMercadoPago,
		Type:       typ,
		ExternalID: "mp-" + strFromInt(env.ID),
		Plan:       plan,
		RawPayload: map[string]any{"action": env.Action, "data": env.Data},
		OccurredAt: occurred,
	}, nil
}

func strFromInt(i int64) string {
	if i == 0 {
		return uuid.NewString()
	}
	// Cheap int→str without strconv import elsewhere in the file.
	const digits = "0123456789"
	if i < 0 {
		return "-" + strFromInt(-i)
	}
	if i == 0 {
		return "0"
	}
	buf := make([]byte, 0, 16)
	for i > 0 {
		buf = append([]byte{digits[i%10]}, buf...)
		i /= 10
	}
	return string(buf)
}
