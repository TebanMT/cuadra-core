package payments

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	"github.com/google/uuid"
	stripe "github.com/stripe/stripe-go/v82"
)

type launchTransport func(*http.Request) (*http.Response, error)

func (f launchTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func stripeTestGateway(fn launchTransport) *StripeGateway {
	g := NewStripeGateway(StripeConfig{SecretKey: "sk_test_local", PriceStandard: "price_future", PriceStandardAnnual: "price_future_annual", PriceStandardAnnualOXXO: "price_future_oxxo"})
	g.client = stripe.NewClient("sk_test_local", stripe.WithBackends(stripe.NewBackendsWithConfig(&stripe.BackendConfig{HTTPClient: &http.Client{Transport: fn}, MaxNetworkRetries: stripe.Int64(0)})))
	return g
}
func TestLaunchStripeCheckoutWirePrices(t *testing.T) {
	for _, tc := range []struct{ plan, method, amount, interval, mode string }{{"standard_monthly", "card", "79900", "month", "subscription"}, {"standard_annual", "card", "720000", "year", "subscription"}, {"standard_annual", "oxxo", "720000", "", "payment"}} {
		t.Run(tc.plan+tc.method, func(t *testing.T) {
			var form url.Values
			g := stripeTestGateway(func(r *http.Request) (*http.Response, error) {
				b, _ := io.ReadAll(r.Body)
				form, _ = url.ParseQuery(string(b))
				if r.URL.Path != "/v1/checkout/sessions" {
					t.Errorf("path %s", r.URL.Path)
				}
				return jsonResponse(`{"id":"cs_test","url":"https://example.test/checkout"}`), nil
			})
			gym := uuid.New()
			_, err := g.StartCheckout(context.Background(), subDomain.CheckoutRequest{GymID: gym, Plan: tc.plan, PaymentMethod: tc.method, LaunchPricing: true, SuccessURL: "https://example.test/success", CancelURL: "https://example.test/cancel"})
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{"mode": tc.mode, "line_items[0][price_data][unit_amount]": tc.amount, "line_items[0][price_data][currency]": "mxn", "line_items[0][price_data][recurring][interval]": tc.interval, "metadata[plan]": tc.plan, "metadata[gym_id]": gym.String(), "line_items[0][price]": ""} {
				if got := form.Get(key); got != want {
					t.Errorf("%s=%q want %q", key, got, want)
				}
			}
		})
	}
}
func TestLaunchPortalCannotReplacePriceAndCachesConfiguration(t *testing.T) {
	calls := 0
	g := stripeTestGateway(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		switch r.URL.Path {
		case "/v1/billing_portal/configurations":
			calls++
			if form.Get("features[subscription_update][enabled]") != "false" || form.Get("features[subscription_cancel][mode]") != "at_period_end" || form.Get("features[payment_method_update][enabled]") != "true" {
				t.Errorf("unsafe portal: %v", form)
			}
			return jsonResponse(`{"id":"bpc_launch"}`), nil
		case "/v1/billing_portal/sessions":
			if form.Get("configuration") != "bpc_launch" {
				t.Errorf("missing protected configuration: %v", form)
			}
			return jsonResponse(`{"url":"https://example.test/portal"}`), nil
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
			return nil, nil
		}
	})
	for i := 0; i < 2; i++ {
		if _, err := g.StartBillingPortal(context.Background(), subDomain.BillingPortalRequest{CustomerID: "cus_test", LaunchPricing: true}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("configuration created %d times", calls)
	}
}
func TestMercadoPagoVerifiesActualPayment(t *testing.T) {
	gym := uuid.NewString()
	status := "pending"
	r := NewMercadoPagoEvents("test_local")
	r.client = &http.Client{Transport: launchTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer test_local" {
			t.Error("missing provider authorization")
		}
		switch req.URL.Path {
		case "/authorized_payments/invoice1":
			return jsonResponse(`{"external_reference":"` + gym + `","payment":{"id":123,"status":"approved"}}`), nil
		case "/v1/payments/123":
			return jsonResponse(`{"id":123,"status":"` + status + `","external_reference":"` + gym + `","transaction_amount":799,"currency_id":"MXN","date_approved":"2026-10-14T15:00:00Z"}`), nil
		case "/preapproval/pre1":
			return jsonResponse(`{"status":"authorized","external_reference":"` + gym + `"}`), nil
		default:
			t.Fatalf("unexpected provider path: %s", req.URL.Path)
			return nil, nil
		}
	})}
	ctx := context.Background()
	for _, kind := range []string{"payment", "subscription_authorized_payment", "subscription_preapproval"} {
		id := map[string]string{"payment": "123", "subscription_authorized_payment": "invoice1", "subscription_preapproval": "pre1"}[kind]
		event, err := r.Resolve(ctx, kind, id)
		if err != nil || event != nil {
			t.Fatalf("unpaid %s event=%+v err=%v", kind, event, err)
		}
	}
	status = "approved"
	direct, err := r.Resolve(ctx, "payment", "123")
	if err != nil || direct == nil {
		t.Fatal(err)
	}
	viaInvoice, err := r.Resolve(ctx, "subscription_authorized_payment", "invoice1")
	if err != nil || viaInvoice == nil {
		t.Fatal(err)
	}
	if direct.ExternalID != viaInvoice.ExternalID || direct.GymID.String() != gym || direct.RawPayload["payment_confirmed"] != true || *direct.Amount != 799 {
		t.Fatalf("wrong confirmed event %+v", direct)
	}
	if _, err := r.Resolve(ctx, "payment", "../../secrets"); err == nil {
		t.Fatal("accepted invalid resource")
	}
}
func TestMercadoPagoLaunchPriceIgnoresFuturePublicPrice(t *testing.T) {
	g := NewMercadoPagoGateway(MercadoPagoConfig{AccessToken: "test_local", AmountStandard: 1299})
	g.http = &http.Client{Transport: launchTransport(func(r *http.Request) (*http.Response, error) {
		var body preapprovalRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.AutoRecurring.TransactionAmount != 799 || body.AutoRecurring.CurrencyID != "MXN" {
			t.Errorf("price %+v", body.AutoRecurring)
		}
		return jsonResponse(`{"id":"pre_test","init_point":"https://example.test/pay"}`), nil
	})}
	if _, err := g.StartCheckout(context.Background(), subDomain.CheckoutRequest{GymID: uuid.New(), Plan: "standard_monthly", LaunchPricing: true}); err != nil {
		t.Fatal(err)
	}
}

func TestMercadoPagoReferenceKeepsPaidSKU(t *testing.T) {
	id := uuid.New()
	for _, plan := range []string{"standard_monthly", "plus_monthly"} {
		got, sku, err := mpGymReference(id.String() + ":" + plan)
		if err != nil || got != id || sku != plan {
			t.Fatalf("reference: id=%s plan=%s err=%v", got, sku, err)
		}
	}
	got, sku, err := mpGymReference(id.String())
	if err != nil || got != id || sku != "" {
		t.Fatal("legacy reference changed")
	}
	if _, _, err := mpGymReference(id.String() + ":trial"); err == nil {
		t.Fatal("invalid paid SKU accepted")
	}
}
