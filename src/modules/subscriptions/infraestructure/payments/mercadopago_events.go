package payments

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	"github.com/google/uuid"
)

var mpResourceID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type MercadoPagoEvents struct {
	token, baseURL string
	client         *http.Client
}

func NewMercadoPagoEvents(token string) *MercadoPagoEvents {
	if token == "" {
		return nil
	}
	return &MercadoPagoEvents{token: token, baseURL: "https://api.mercadopago.com", client: &http.Client{Timeout: 15 * time.Second}}
}
func (r *MercadoPagoEvents) get(ctx context.Context, path string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("mercadopago resource status %d", resp.StatusCode)
	}
	var out map[string]any
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
func mpString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func (r *MercadoPagoEvents) Resolve(ctx context.Context, kind, id string) (*subDomain.Event, error) {
	if !mpResourceID.MatchString(id) {
		return nil, fmt.Errorf("invalid Mercado Pago resource id")
	}
	var path string
	switch kind {
	case "payment":
		path = "/v1/payments/" + id
	case "subscription_authorized_payment":
		path = "/authorized_payments/" + id
	case "subscription_preapproval":
		path = "/preapproval/" + id
	default:
		return nil, nil
	}
	obj, err := r.get(ctx, path)
	if err != nil {
		return nil, err
	}
	ref := mpString(obj["external_reference"])
	if kind == "subscription_preapproval" {
		if obj["status"] != "cancelled" {
			return nil, nil
		}
		gym, _, err := mpGymReference(ref)
		if err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339, mpString(obj["last_modified"]))
		if err != nil {
			return nil, err
		}
		return &subDomain.Event{GymID: gym, Provider: subDomain.ProviderMercadoPago, Type: subDomain.EventCancelled, ExternalID: "mp-cancel-" + id + "-" + at.UTC().Format(time.RFC3339Nano), OccurredAt: at, RawPayload: map[string]any{"resource": obj}}, nil
	}
	if kind == "subscription_authorized_payment" {
		payment, _ := obj["payment"].(map[string]any)
		if payment["status"] != "approved" {
			return nil, nil
		}
		paymentID := mpString(payment["id"])
		if !mpResourceID.MatchString(paymentID) {
			return nil, fmt.Errorf("missing authorized payment id")
		}
		obj, err = r.get(ctx, "/v1/payments/"+paymentID)
		if err != nil {
			return nil, err
		}
	}
	if obj["status"] != "approved" {
		return nil, nil
	}
	if pRef := mpString(obj["external_reference"]); pRef != "" {
		ref = pRef
	}
	gym, plan, err := mpGymReference(ref)
	if err != nil {
		return nil, fmt.Errorf("payment has no gym reference")
	}
	number, ok := obj["transaction_amount"].(json.Number)
	if !ok {
		return nil, fmt.Errorf("missing payment amount")
	}
	amount, err := number.Float64()
	if err != nil || amount <= 0 {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, mpString(obj["date_approved"]))
	if err != nil {
		return nil, err
	}
	currency := strings.ToUpper(mpString(obj["currency_id"]))
	end := at.AddDate(0, 1, 0)
	return &subDomain.Event{GymID: gym, Provider: subDomain.ProviderMercadoPago, Type: subDomain.EventRenewed, ExternalID: "mp-payment-" + mpString(obj["id"]), Plan: plan, Amount: &amount, Currency: &currency, PeriodEndsAt: &end, OccurredAt: at, RawPayload: map[string]any{"payment_confirmed": true, "resource": obj}}, nil
}

// New checkouts carry the paid SKU in the provider-owned reference. Legacy
// UUID-only references use the gym's existing SKU in RecordEvent.
func mpGymReference(ref string) (uuid.UUID, string, error) {
	parts := strings.SplitN(ref, ":", 2)
	id, err := uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, "", err
	}
	if len(parts) == 1 {
		return id, "", nil
	}
	if parts[1] != gymDomain.PlanStandardMonthly && parts[1] != gymDomain.PlanPlusMonthly {
		return uuid.Nil, "", fmt.Errorf("unsupported Mercado Pago plan")
	}
	return id, parts[1], nil
}
