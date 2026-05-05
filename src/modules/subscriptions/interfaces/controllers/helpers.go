package controllers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	stripewebhook "github.com/stripe/stripe-go/v82/webhook"
)

// WebhookVerifier validates incoming webhook bodies against the per-provider
// secret. Stripe verification delegates to stripe-go's webhook.ConstructEvent,
// which handles timestamp tolerance + multiple `v1=` signatures (rotation).
// Mercado Pago keeps its inline HMAC since MP has no Go SDK helper.
//
// Behaviour:
//   * If a secret is empty (env var unset), verification *passes* in
//     development. This unblocks local testing before keys are provisioned.
//   * In production (StrictDev=true) an empty secret rejects every request.
type WebhookVerifier struct {
	StripeSecret      string
	MercadoPagoSecret string
	StrictDev         bool
}

func NewWebhookVerifier(stripe, mp string, strictDev bool) *WebhookVerifier {
	return &WebhookVerifier{
		StripeSecret:      stripe,
		MercadoPagoSecret: mp,
		StrictDev:         strictDev,
	}
}

// VerifyStripe delegates to github.com/stripe/stripe-go/v82/webhook —
// timestamp tolerance, signature rotation, and constant-time comparison are
// handled there. The parsed event is intentionally discarded: the controller
// re-decodes the body into our own envelope (see parseStripeEvent) so the
// shape we depend on lives in our code, not the SDK.
func (v *WebhookVerifier) VerifyStripe(body []byte, header string) error {
	if v.StripeSecret == "" {
		if v.StrictDev {
			return errors.New("stripe webhook secret not configured")
		}
		return nil
	}
	if header == "" {
		return errors.New("missing stripe-signature header")
	}
	if _, err := stripewebhook.ConstructEvent(body, header, v.StripeSecret); err != nil {
		return fmt.Errorf("stripe webhook verification: %w", err)
	}
	return nil
}

// VerifyMercadoPago implements the simplified `x-signature: ts=...,v1=...`
// scheme MP uses for webhooks. The string-to-sign is documented as
// `id:<data.id>;request-id:<request-id>;ts:<ts>;`.
func (v *WebhookVerifier) VerifyMercadoPago(body []byte, signatureHeader, requestID, dataID string) error {
	if v.MercadoPagoSecret == "" {
		if v.StrictDev {
			return errors.New("mercadopago webhook secret not configured")
		}
		return nil
	}
	if signatureHeader == "" {
		return errors.New("missing x-signature header")
	}
	parts := strings.Split(signatureHeader, ",")
	timestamp := ""
	v1 := ""
	for _, p := range parts {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "ts":
			timestamp = kv[1]
		case "v1":
			v1 = kv[1]
		}
	}
	if timestamp == "" || v1 == "" {
		return errors.New("malformed x-signature header")
	}
	manifest := fmt.Sprintf("id:%s;request-id:%s;ts:%s;", dataID, requestID, timestamp)
	mac := hmac.New(sha256.New, []byte(v.MercadoPagoSecret))
	mac.Write([]byte(manifest))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(v1)) {
		// `body` is captured for future verbose-logging. Avoid logging the
		// full body in this error — it may contain PII.
		_ = body
		return errors.New("mercadopago signature mismatch")
	}
	return nil
}
