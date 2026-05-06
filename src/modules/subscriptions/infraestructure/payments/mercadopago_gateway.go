package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
)

// MercadoPagoConfig holds the bits the MP preapproval adapter needs.
//
// Cuadra uses MP's "preapproval" flow (recurring authorisation against the
// payer's card) — *not* the marketplace "preapproval_plan" flavour, because
// the plan flavour requires the payer to pick a public plan from MP's UI and
// gives us less control over success/cancel routing.
//
// We let the founder configure the recurring price per gym plan (in MXN). The
// alternative — sending a `preapproval_plan_id` — is fine but harder to
// preview during testing, so we keep it inline.
type MercadoPagoConfig struct {
	AccessToken    string
	AmountStandard float64 // MXN per month for "pro_monthly"
	AmountPlus     float64 // MXN per month for "pro_annual"
	BackURL        string  // post-checkout redirect (success + cancel use the same)
}

// MercadoPagoGateway implements subDomain.CheckoutGateway by hitting MP's
// `/preapproval` endpoint directly. We deliberately skip the (incomplete) Go
// SDK and use net/http — fewer dependencies, easier to debug.
type MercadoPagoGateway struct {
	cfg     MercadoPagoConfig
	amounts map[string]float64
	http    *http.Client
	baseURL string
}

func NewMercadoPagoGateway(cfg MercadoPagoConfig) *MercadoPagoGateway {
	if cfg.AccessToken == "" {
		return nil
	}
	amounts := map[string]float64{}
	if cfg.AmountStandard > 0 {
		amounts[gymDomain.PlanProMonthly] = cfg.AmountStandard
	}
	if cfg.AmountPlus > 0 {
		amounts[gymDomain.PlanProAnnual] = cfg.AmountPlus
	}
	return &MercadoPagoGateway{
		cfg:     cfg,
		amounts: amounts,
		http:    &http.Client{Timeout: 15 * time.Second},
		baseURL: "https://api.mercadopago.com",
	}
}

func (g *MercadoPagoGateway) Provider() subDomain.Provider { return subDomain.ProviderMercadoPago }

// preapprovalRequest is the subset of MP's /preapproval body we send. We
// inline-decode the JSON shape rather than importing a SDK to avoid a heavy
// transitive dep tree for a single endpoint.
type preapprovalRequest struct {
	Reason            string                   `json:"reason"`
	ExternalReference string                   `json:"external_reference"`
	PayerEmail        string                   `json:"payer_email,omitempty"`
	BackURL           string                   `json:"back_url"`
	Status            string                   `json:"status"` // "pending" — payer authorises in MP UI
	AutoRecurring     preapprovalAutoRecurring `json:"auto_recurring"`
}

type preapprovalAutoRecurring struct {
	Frequency         int     `json:"frequency"`
	FrequencyType     string  `json:"frequency_type"` // "months"
	TransactionAmount float64 `json:"transaction_amount"`
	CurrencyID        string  `json:"currency_id"` // "MXN"
}

type preapprovalResponse struct {
	ID        string `json:"id"`
	InitPoint string `json:"init_point"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	Error     string `json:"error"`
}

func (g *MercadoPagoGateway) StartCheckout(ctx context.Context, in subDomain.CheckoutRequest) (subDomain.CheckoutResult, error) {
	amount, ok := g.amounts[in.Plan]
	if !ok {
		return subDomain.CheckoutResult{}, fmt.Errorf("mercadopago: %w (plan=%s)", subDomain.ErrUnsupportedPlan, in.Plan)
	}
	reason := "Cuadra — suscripción mensual"
	if in.GymName != "" {
		reason = fmt.Sprintf("Cuadra — %s", in.GymName)
	}
	body := preapprovalRequest{
		Reason:            reason,
		ExternalReference: in.GymID.String(),
		PayerEmail:        in.CustomerEmail,
		BackURL:           backOrFallback(g.cfg.BackURL, in.SuccessURL),
		Status:            "pending",
		AutoRecurring: preapprovalAutoRecurring{
			Frequency:         1,
			FrequencyType:     "months",
			TransactionAmount: amount,
			CurrencyID:        "MXN",
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return subDomain.CheckoutResult{}, fmt.Errorf("mercadopago: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/preapproval", bytes.NewReader(buf))
	if err != nil {
		return subDomain.CheckoutResult{}, fmt.Errorf("mercadopago: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		return subDomain.CheckoutResult{}, fmt.Errorf("mercadopago: send request: %w", err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return subDomain.CheckoutResult{}, fmt.Errorf("mercadopago: status %d: %s", resp.StatusCode, string(respBytes))
	}
	var out preapprovalResponse
	if err := json.Unmarshal(respBytes, &out); err != nil {
		return subDomain.CheckoutResult{}, fmt.Errorf("mercadopago: decode response: %w", err)
	}
	if out.InitPoint == "" {
		return subDomain.CheckoutResult{}, errors.New("mercadopago: empty init_point")
	}
	return subDomain.CheckoutResult{URL: out.InitPoint, SessionID: out.ID}, nil
}

func backOrFallback(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
