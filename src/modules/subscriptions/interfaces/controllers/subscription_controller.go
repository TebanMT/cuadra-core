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
	Verifier *WebhookVerifier
	Tokens   auth.TokenService
}

func NewSubscriptionController(record *subApp.RecordEvent, get *subApp.GetSubscription, checkout *subApp.StartCheckout, verifier *WebhookVerifier, tokens auth.TokenService) *SubscriptionController {
	return &SubscriptionController{Record: record, Get: get, Checkout: checkout, Verifier: verifier, Tokens: tokens}
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
	}
	wh := r.Group("/api/v1/webhooks")
	wh.POST("/stripe", ctrl.handleStripe)
	wh.POST("/mercadopago", ctrl.handleMercadoPago)
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
		hist = append(hist, subscriptionEvWi{
			ID:         e.ID,
			Provider:   string(e.Provider),
			Type:       string(e.Type),
			Plan:       e.Plan,
			Amount:     e.Amount,
			Currency:   e.Currency,
			OccurredAt: e.OccurredAt,
			PeriodEnd:  e.PeriodEndsAt,
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
	Provider string `json:"provider"` // "stripe" | "mercadopago"
	Plan     string `json:"plan"`     // "pro_monthly" | "pro_annual"
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
	out, err := ctrl.Checkout.Execute(c.Request.Context(), subApp.StartCheckoutInput{
		GymID:    gymID,
		UserID:   userID,
		Provider: subDomain.Provider(provider),
		Plan:     plan,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, startCheckoutResp{URL: out.URL, SessionID: out.SessionID})
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
	if gymID == uuid.Nil {
		// We need to know which gym this concerns. Stripe's `client_reference_id`
		// or `metadata.gym_id` carries it in our setup — if missing, drop the
		// event (returning nil bypasses RecordEvent).
		return nil, nil
	}
	var typ subDomain.EventType
	switch env.Type {
	case "customer.subscription.created", "invoice.payment_succeeded":
		typ = subDomain.EventActivated
	case "invoice.paid":
		typ = subDomain.EventRenewed
	case "invoice.payment_failed":
		typ = subDomain.EventPastDue
	case "customer.subscription.deleted":
		typ = subDomain.EventCancelled
	default:
		return nil, nil
	}
	occurred := time.Unix(env.Created, 0).UTC()
	if env.Created == 0 {
		occurred = time.Now().UTC()
	}
	return &subApp.RecordEventInput{
		GymID:        gymID,
		Provider:     subDomain.ProviderStripe,
		Type:         typ,
		ExternalID:   env.ID,
		Plan:         plan,
		Amount:       amount,
		Currency:     currency,
		PeriodEndsAt: periodEnd,
		RawPayload:   map[string]any{"event": env.Type, "data": env.Data},
		OccurredAt:   occurred,
	}, nil
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
	if items, ok := obj["items"].(map[string]any); ok {
		if datas, ok := items["data"].([]any); ok && len(datas) > 0 {
			if first, ok := datas[0].(map[string]any); ok {
				if p, ok := first["plan"].(map[string]any); ok {
					if nick, ok := p["nickname"].(string); ok {
						plan = nick
					}
				}
			}
		}
	}
	if plan == "" {
		// Fallback: monthly is the default.
		plan = "pro_monthly"
	}
	var periodEnd *time.Time
	if pe, ok := obj["current_period_end"].(float64); ok && pe > 0 {
		t := time.Unix(int64(pe), 0).UTC()
		periodEnd = &t
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
	if env.Type != "subscription_preapproval" && env.Type != "payment" {
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
		typ = subDomain.EventActivated
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
	plan := "pro_monthly"
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
