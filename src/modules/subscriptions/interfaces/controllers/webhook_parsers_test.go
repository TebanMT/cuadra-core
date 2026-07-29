package controllers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
)

// ── MercadoPago parser ───────────────────────────────────────────────────

func mpEnvelopeJSON(t *testing.T, env map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestParseMP_FirstPaymentMapsToActivated(t *testing.T) {
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":           int64(123),
		"type":         "payment",
		"action":       "payment.created",
		"date_created": "2026-05-20T12:00:00Z",
		"data": map[string]any{
			"id":                 "pay_001",
			"external_reference": gymID.String(),
		},
	})
	in, err := parseMercadoPagoEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventActivated {
		t.Errorf("type=%q, want %q", in.Type, subDomain.EventActivated)
	}
	if in.GymID != gymID {
		t.Errorf("gym_id mismatch")
	}
	if in.ExternalID != "mp-123" {
		t.Errorf("external_id=%q, want mp-123", in.ExternalID)
	}
}

func TestParseMP_AuthorizedPaymentMapsToRenewed(t *testing.T) {
	// The bug we're fixing: MP emits `subscription_authorized_payment` for
	// each recurring charge of an active preapproval. Before the fix the
	// parser silently dropped these → the gym never extended its
	// subscription_ends_at and eventually got past_due'd by the gate.
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":           int64(456),
		"type":         "subscription_authorized_payment",
		"action":       "subscription_authorized_payment.created",
		"date_created": "2026-06-20T12:00:00Z",
		"data": map[string]any{
			"id":                 "auth_pay_001",
			"external_reference": gymID.String(),
		},
	})
	in, err := parseMercadoPagoEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventRenewed {
		t.Errorf("type=%q, want %q (renewed)", in.Type, subDomain.EventRenewed)
	}
}

func TestParseMP_Cancelled(t *testing.T) {
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":     int64(789),
		"type":   "subscription_preapproval",
		"action": "subscription_preapproval.cancelled",
		"data": map[string]any{
			"id":                 "sub_001",
			"external_reference": gymID.String(),
		},
	})
	in, err := parseMercadoPagoEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventCancelled {
		t.Errorf("type=%q, want %q", in.Type, subDomain.EventCancelled)
	}
}

func TestParseMP_UnknownTypeIgnored(t *testing.T) {
	body := mpEnvelopeJSON(t, map[string]any{
		"id":     int64(1),
		"type":   "merchant_order", // not subscription-related
		"action": "anything",
	})
	in, err := parseMercadoPagoEvent(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if in != nil {
		t.Fatalf("expected nil (ignored), got %+v", in)
	}
}

func TestParseMP_MissingExternalReferenceIgnored(t *testing.T) {
	// No external_reference → we can't identify the gym; drop silently so
	// Stripe/MP get a 200 and stop retrying noise we can't act on.
	body := mpEnvelopeJSON(t, map[string]any{
		"id":     int64(1),
		"type":   "payment",
		"action": "payment.created",
		"data":   map[string]any{},
	})
	in, err := parseMercadoPagoEvent(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if in != nil {
		t.Fatalf("expected nil (no gym id), got %+v", in)
	}
}

// ── Stripe parser ────────────────────────────────────────────────────────

// stripeRenewalInvoice arma un payload de invoice con la forma REAL que manda
// Stripe en renovaciones (API 2025-03-31 Basil en adelante, incl. dahlia):
//   - object.metadata VACÍA (la invoice NO hereda la metadata de la
//     subscription — bug jul-2026: asumir que sí la hereda hizo que toda
//     renovación se descartara en silencio),
//   - gym_id en parent.subscription_details.metadata,
//   - fin de periodo en lines.data[0].period.end (no hay current_period_end),
//   - línea con `pricing` sin nickname.
func stripeRenewalInvoice(gymID uuid.UUID, periodEnd int64) map[string]any {
	return map[string]any{
		"id":             "in_renewal_001",
		"object":         "invoice",
		"metadata":       map[string]any{},
		"billing_reason": "subscription_cycle",
		"customer":       "cus_ABC123",
		"amount_paid":    float64(79900), // $799.00 MXN en centavos
		"currency":       "mxn",
		"parent": map[string]any{
			"type": "subscription_details",
			"subscription_details": map[string]any{
				"subscription": "sub_XYZ",
				"metadata":     map[string]any{"gym_id": gymID.String()},
			},
		},
		"lines": map[string]any{
			"data": []any{
				map[string]any{
					"period": map[string]any{
						"start": float64(periodEnd - 30*24*3600),
						"end":   float64(periodEnd),
					},
					"pricing": map[string]any{
						"price_details": map[string]any{"price": "price_123"},
					},
				},
			},
		},
	}
}

func TestParseStripe_InvoicePaidRenewal_ModernAPI(t *testing.T) {
	// Renovación mensual real: el gym_id viene en
	// parent.subscription_details.metadata, NO en object.metadata.
	gymID := uuid.New()
	periodEnd := int64(1756080000) // 2025-08-25
	body := mpEnvelopeJSON(t, map[string]any{
		"id":      "evt_renew",
		"type":    "invoice.paid",
		"created": int64(1753488000), // 2025-07-26
		"data":    map[string]any{"object": stripeRenewalInvoice(gymID, periodEnd)},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventRenewed {
		t.Errorf("type=%q, want renewed", in.Type)
	}
	if in.GymID != gymID {
		t.Errorf("gym_id mismatch — no se leyó parent.subscription_details.metadata")
	}
	if in.PeriodEndsAt == nil || in.PeriodEndsAt.Unix() != periodEnd {
		t.Errorf("period_ends_at=%v, want unix=%d (lines.data[0].period.end)", in.PeriodEndsAt, periodEnd)
	}
	if in.Plan != "" {
		t.Errorf("plan=%q, want \"\" (sin nickname en la línea debe conservar el plan del gym, no caer a standard_monthly)", in.Plan)
	}
	if in.ExternalID != "in_renewal_001" {
		t.Errorf("external_id=%q, want in_renewal_001 (invoice id, para dedupe paid/payment_succeeded)", in.ExternalID)
	}
	if in.Amount == nil || *in.Amount != 799.00 {
		t.Errorf("amount=%v, want 799.00", in.Amount)
	}
	if in.StripeCustomerID != "cus_ABC123" {
		t.Errorf("stripe_customer_id=%q, want cus_ABC123", in.StripeCustomerID)
	}
}

func TestParseStripe_InvoicePaidRenewal_LegacyAPI(t *testing.T) {
	// API pre-Basil (2023-08-16 .. 2025-02): la metadata de la subscription
	// viaja en subscription_details.metadata (sin `parent`) y la línea trae
	// plan.nickname — de ahí sí podemos extraer el plan.
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":      "evt_renew_legacy",
		"type":    "invoice.paid",
		"created": int64(1753488000),
		"data": map[string]any{
			"object": map[string]any{
				"id":             "in_legacy_001",
				"metadata":       map[string]any{},
				"billing_reason": "subscription_cycle",
				"subscription_details": map[string]any{
					"metadata": map[string]any{"gym_id": gymID.String()},
				},
				"lines": map[string]any{
					"data": []any{
						map[string]any{
							"period": map[string]any{"end": float64(1756080000)},
							"plan":   map[string]any{"nickname": "plus_monthly"},
						},
					},
				},
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.GymID != gymID {
		t.Errorf("gym_id mismatch — no se leyó subscription_details.metadata (legacy)")
	}
	if in.Plan != "plus_monthly" {
		t.Errorf("plan=%q, want plus_monthly (nickname de la línea legacy)", in.Plan)
	}
}

func TestParseStripe_InvoiceFirstChargeIgnored(t *testing.T) {
	// billing_reason=subscription_create es el cobro inicial del checkout —
	// customer.subscription.created ya registra la activación; procesar
	// también su invoice duplicaría la fila en la historia.
	obj := stripeRenewalInvoice(uuid.New(), 1756080000)
	obj["billing_reason"] = "subscription_create"
	body := mpEnvelopeJSON(t, map[string]any{
		"id":   "evt_first_invoice",
		"type": "invoice.paid",
		"data": map[string]any{"object": obj},
	})
	in, err := parseStripeEvent(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if in != nil {
		t.Fatalf("expected nil (first charge handled by subscription.created), got %+v", in)
	}
}

func TestParseStripe_InvoicePaymentSucceededSharesInvoiceExternalID(t *testing.T) {
	// invoice.payment_succeeded e invoice.paid disparan por el MISMO cobro con
	// event ids (evt_...) distintos. Ambos deben usar la invoice (in_...) como
	// external id para que la idempotencia (provider, external_id) los colapse
	// en una sola fila, llegue el que llegue primero.
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":      "evt_pay_succeeded",
		"type":    "invoice.payment_succeeded",
		"created": int64(1753488000),
		"data":    map[string]any{"object": stripeRenewalInvoice(gymID, 1756080000)},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventRenewed {
		t.Errorf("type=%q, want renewed", in.Type)
	}
	if in.ExternalID != "in_renewal_001" {
		t.Errorf("external_id=%q, want in_renewal_001 (mismo que invoice.paid)", in.ExternalID)
	}
}

func TestParseStripe_InvoicePaymentFailedKeepsEventID(t *testing.T) {
	// payment_failed conserva el evt_... : cada reintento de dunning fallido
	// es un evento distinto, y el in_... queda libre para que un invoice.paid
	// tardío (el dueño arregló la tarjeta) no choque contra el fallo previo.
	gymID := uuid.New()
	obj := stripeRenewalInvoice(gymID, 1756080000)
	obj["amount_paid"] = float64(0)
	body := mpEnvelopeJSON(t, map[string]any{
		"id":      "evt_pay_failed_1",
		"type":    "invoice.payment_failed",
		"created": int64(1753488000),
		"data":    map[string]any{"object": obj},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventPastDue {
		t.Errorf("type=%q, want past_due", in.Type)
	}
	if in.ExternalID != "evt_pay_failed_1" {
		t.Errorf("external_id=%q, want evt_pay_failed_1 (event id, no invoice id)", in.ExternalID)
	}
	if in.GymID != gymID {
		t.Errorf("gym_id mismatch")
	}
}

func TestParseStripe_SubscriptionCreatedAnnualNickname(t *testing.T) {
	// Cuando el dueño compra Standard anual, Stripe emite
	// customer.subscription.created con el item del price recurrente anual.
	// El nickname del price (configurado como "standard_annual" en el Stripe
	// dashboard) baja por items.data[0].plan.nickname y debe terminar en
	// RecordEventInput.Plan para que ActivateSubscription lo asiente sin
	// caer al default "standard_monthly".
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":      "evt_sub_annual",
		"type":    "customer.subscription.created",
		"created": int64(1716206400),
		"data": map[string]any{
			"object": map[string]any{
				"metadata": map[string]any{"gym_id": gymID.String()},
				"items": map[string]any{
					"data": []any{
						map[string]any{
							"plan": map[string]any{
								"nickname": "standard_annual",
							},
						},
					},
				},
				"current_period_end": float64(1747742400), // 2025-05-20
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventActivated {
		t.Errorf("type=%q, want activated", in.Type)
	}
	if in.Plan != "standard_annual" {
		t.Errorf("plan=%q, want standard_annual (cayó al default mensual)", in.Plan)
	}
	if in.GymID != gymID {
		t.Errorf("gym_id mismatch")
	}
	if in.PeriodEndsAt == nil {
		t.Errorf("period_ends_at nil — el current_period_end no se mapeó")
	}
}

func TestParseStripe_SubscriptionCreated_ModernAPIPeriodEndInItems(t *testing.T) {
	// API version 2026-04-22.dahlia (y otras 2025-09+) movieron
	// current_period_end del root del subscription object a
	// items.data[0].current_period_end. El parser debe leer de ambos lugares
	// para que webhooks de cuentas Stripe nuevas (que reciben la API más
	// reciente por default) plantemos un period_ends_at correcto y el
	// dashboard no muestre "Sin fecha establecida".
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":      "evt_sub_modern",
		"type":    "customer.subscription.created",
		"created": int64(1716206400),
		"data": map[string]any{
			"object": map[string]any{
				"metadata": map[string]any{"gym_id": gymID.String()},
				"items": map[string]any{
					"data": []any{
						map[string]any{
							"plan": map[string]any{
								"nickname": "standard_monthly",
							},
							"current_period_end": float64(1747742400), // 2025-05-20
						},
					},
				},
				// NO current_period_end en el root — emulando la API moderna.
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.PeriodEndsAt == nil {
		t.Fatal("PeriodEndsAt nil — el parser no leyó items.data[0].current_period_end")
	}
	wantUnix := int64(1747742400)
	if in.PeriodEndsAt.Unix() != wantUnix {
		t.Errorf("PeriodEndsAt unix=%d, want %d", in.PeriodEndsAt.Unix(), wantUnix)
	}
}

func TestParseStripe_SubscriptionUpdated_CancelScheduled(t *testing.T) {
	// El dueño cancela desde el Stripe Customer Portal (cancel at period end).
	// Stripe NO emite subscription.deleted ahora — emite subscription.updated
	// con cancel_at_period_end=true y previous_attributes.cancel_at_period_end
	// presente. Mapeamos a EventCancelled para reflejar el estado al dashboard;
	// el grace period vive en SubscriptionEndsAt (no lo movemos en Cancel()).
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":   "evt_sub_cancel_scheduled",
		"type": "customer.subscription.updated",
		"data": map[string]any{
			"object": map[string]any{
				"metadata":             map[string]any{"gym_id": gymID.String()},
				"cancel_at_period_end": true,
				"current_period_end":   float64(1747742400),
			},
			"previous_attributes": map[string]any{
				"cancel_at_period_end": false,
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventCancelled {
		t.Errorf("type=%q, want cancelled", in.Type)
	}
}

func TestParseStripe_SubscriptionUpdated_Reactivated(t *testing.T) {
	// El dueño se arrepintió y reactivó desde el portal.
	// cancel_at_period_end pasa true → false. Mapeamos a EventActivated.
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":   "evt_sub_reactivated",
		"type": "customer.subscription.updated",
		"data": map[string]any{
			"object": map[string]any{
				"metadata":             map[string]any{"gym_id": gymID.String()},
				"cancel_at_period_end": false,
				"items": map[string]any{
					"data": []any{
						map[string]any{
							"plan":               map[string]any{"nickname": "standard_monthly"},
							"current_period_end": float64(1747742400),
						},
					},
				},
			},
			"previous_attributes": map[string]any{
				"cancel_at_period_end": true,
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventActivated {
		t.Errorf("type=%q, want activated", in.Type)
	}
}

func TestParseStripe_SubscriptionUpdated_UnrelatedChangeIgnored(t *testing.T) {
	// El dueño cambió tarjeta (default_payment_method en previous_attributes).
	// No tocó cancel_at_period_end, así que no es una transición que nos
	// importe — devolvemos nil para que Stripe reciba 200 sin record.
	body := mpEnvelopeJSON(t, map[string]any{
		"id":   "evt_sub_card_change",
		"type": "customer.subscription.updated",
		"data": map[string]any{
			"object": map[string]any{
				"metadata":               map[string]any{"gym_id": uuid.NewString()},
				"default_payment_method": "pm_new",
				"cancel_at_period_end":   false,
			},
			"previous_attributes": map[string]any{
				"default_payment_method": "pm_old",
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if in != nil {
		t.Fatalf("expected nil (no cancel transition), got %+v", in)
	}
}

func TestParseStripe_SubscriptionDeletedIsCancelled(t *testing.T) {
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":   "evt_del",
		"type": "customer.subscription.deleted",
		"data": map[string]any{
			"object": map[string]any{
				"metadata": map[string]any{"gym_id": gymID.String()},
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v", err)
	}
	if in.Type != subDomain.EventCancelled {
		t.Errorf("type=%q, want cancelled", in.Type)
	}
}

// ── Stripe parser: OXXO branch ───────────────────────────────────────────

func TestParseStripe_CheckoutSessionCompletedSubscriptionIgnoredHere(t *testing.T) {
	// El flow Stripe Subscriptions (mode=subscription) NO debe entrar al
	// branch OXXO; se cubre con customer.subscription.created. Si el parser
	// procesa checkout.session.completed mode=subscription, generaría un
	// evento duplicado.
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":      "cs_sub_001",
		"type":    "checkout.session.completed",
		"created": int64(1716206400),
		"data": map[string]any{
			"object": map[string]any{
				"mode":           "subscription",
				"payment_status": "paid",
				"metadata":       map[string]any{"gym_id": gymID.String()},
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if in != nil {
		t.Fatalf("expected nil (mode=subscription handled elsewhere), got %+v", in)
	}
}

func TestParseStripe_OXXOVoucherEmitted(t *testing.T) {
	// El cliente terminó el Checkout en OXXO. Stripe emitió la ficha y
	// envió checkout.session.completed con payment_status=unpaid. Esperamos
	// EventVoucherEmitted con voucher_url + expiry capturados.
	gymID := uuid.New()
	expiresAt := int64(1716465600) // 2024-05-23
	body := mpEnvelopeJSON(t, map[string]any{
		"id":      "cs_oxxo_001",
		"type":    "checkout.session.completed",
		"created": int64(1716206400),
		"data": map[string]any{
			"object": map[string]any{
				"mode":           "payment",
				"payment_status": "unpaid",
				"metadata": map[string]any{
					"gym_id":         gymID.String(),
					"plan":           "standard_annual",
					"payment_method": "oxxo",
				},
				"payment_intent": map[string]any{
					"next_action": map[string]any{
						"oxxo_display_details": map[string]any{
							"expires_after":      float64(expiresAt),
							"hosted_voucher_url": "https://stripe.com/oxxo/ABC",
						},
					},
				},
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventVoucherEmitted {
		t.Errorf("type=%q, want voucher_emitted", in.Type)
	}
	if in.Plan != "standard_annual" {
		t.Errorf("plan=%q, want standard_annual", in.Plan)
	}
	if in.GymID != gymID {
		t.Errorf("gym_id mismatch")
	}
	if in.PeriodEndsAt == nil || in.PeriodEndsAt.Unix() != expiresAt {
		t.Errorf("period_ends_at=%v, want unix=%d (voucher expiry)", in.PeriodEndsAt, expiresAt)
	}
	if got := in.RawPayload["voucher_url"]; got != "https://stripe.com/oxxo/ABC" {
		t.Errorf("voucher_url not captured: got %v", got)
	}
}

func TestParseStripe_PaymentIntentSucceededWithOXXOMetadata(t *testing.T) {
	// Cliente fue al OXXO y pagó. Stripe emite payment_intent.succeeded
	// con metadata.gym_id + metadata.payment_method=oxxo. Esperamos
	// EventActivated con plan=standard_annual + period_ends_at = paid + 1y.
	gymID := uuid.New()
	created := int64(1716465600) // 2024-05-23
	body := mpEnvelopeJSON(t, map[string]any{
		"id":      "evt_pi_oxxo_paid",
		"type":    "payment_intent.succeeded",
		"created": created,
		"data": map[string]any{
			"object": map[string]any{
				"metadata": map[string]any{
					"gym_id":         gymID.String(),
					"plan":           "standard_annual",
					"payment_method": "oxxo",
				},
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventActivated {
		t.Errorf("type=%q, want activated", in.Type)
	}
	if in.Plan != "standard_annual" {
		t.Errorf("plan=%q, want standard_annual", in.Plan)
	}
	if in.PeriodEndsAt == nil {
		t.Fatal("period_ends_at nil — la activación OXXO debe asentar un año adelante")
	}
	wantEnd := time.Unix(created, 0).UTC().AddDate(1, 0, 0)
	if !in.PeriodEndsAt.Equal(wantEnd) {
		t.Errorf("period_ends_at=%v, want %v (paid+1y)", in.PeriodEndsAt, wantEnd)
	}
}

func TestParseStripe_PaymentIntentFailedWithOXXOMetadata(t *testing.T) {
	// El voucher venció sin pagarse. Stripe emite
	// payment_intent.payment_failed. Mapeamos a EventVoucherExpired
	// (NO past_due — past_due es para fallos de tarjeta).
	gymID := uuid.New()
	body := mpEnvelopeJSON(t, map[string]any{
		"id":      "evt_pi_oxxo_failed",
		"type":    "payment_intent.payment_failed",
		"created": int64(1716724800),
		"data": map[string]any{
			"object": map[string]any{
				"metadata": map[string]any{
					"gym_id":         gymID.String(),
					"plan":           "standard_annual",
					"payment_method": "oxxo",
				},
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil || in == nil {
		t.Fatalf("parse err=%v in=%v", err, in)
	}
	if in.Type != subDomain.EventVoucherExpired {
		t.Errorf("type=%q, want voucher_expired", in.Type)
	}
}

func TestParseStripe_PaymentIntentWithoutOXXOMetadataIgnored(t *testing.T) {
	// payment_intent.* sin marker payment_method=oxxo se ignora. Cubre
	// el caso de PIs futuros (ej. tap-to-sell) que no son de OXXO.
	body := mpEnvelopeJSON(t, map[string]any{
		"id":   "evt_pi_random",
		"type": "payment_intent.succeeded",
		"data": map[string]any{
			"object": map[string]any{
				"metadata": map[string]any{"gym_id": uuid.NewString()},
			},
		},
	})
	in, err := parseStripeEvent(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if in != nil {
		t.Fatalf("expected nil (no oxxo marker), got %+v", in)
	}
}

// ── Signature verification ───────────────────────────────────────────────

func TestVerifier_MPSignatureValid(t *testing.T) {
	secret := "shh-this-is-a-test-secret"
	dataID := "pay_001"
	requestID := "req_abc"
	ts := "1716206400"
	manifest := fmt.Sprintf("id:%s;request-id:%s;ts:%s;", dataID, requestID, ts)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(manifest))
	good := hex.EncodeToString(mac.Sum(nil))
	header := "ts=" + ts + ",v1=" + good

	v := NewWebhookVerifier("", secret, true)
	if err := v.VerifyMercadoPago([]byte("ignored"), header, requestID, dataID); err != nil {
		t.Fatalf("valid sig rejected: %v", err)
	}
}

func TestVerifier_MPSignatureTampered(t *testing.T) {
	v := NewWebhookVerifier("", "real-secret", true)
	tampered := "ts=1716206400,v1=" + strings.Repeat("0", 64)
	err := v.VerifyMercadoPago([]byte("ignored"), tampered, "req", "data")
	if err == nil {
		t.Fatal("tampered signature accepted")
	}
}

func TestVerifier_MPMissingHeader(t *testing.T) {
	v := NewWebhookVerifier("", "real-secret", true)
	if err := v.VerifyMercadoPago([]byte("body"), "", "req", "data"); err == nil {
		t.Fatal("missing header should reject")
	}
}

func TestVerifier_MPEmptySecretRejectsInProduction(t *testing.T) {
	// StrictDev=true ⇔ ENVIRONMENT=production. Sin secret en prod, todo
	// se rechaza para no aceptar webhooks no firmados.
	v := NewWebhookVerifier("", "", true)
	if err := v.VerifyMercadoPago([]byte("body"), "ts=1,v1=x", "req", "data"); err == nil {
		t.Fatal("empty secret in production should reject")
	}
}

func TestVerifier_MPEmptySecretAcceptsInDev(t *testing.T) {
	v := NewWebhookVerifier("", "", false)
	if err := v.VerifyMercadoPago([]byte("body"), "anything", "req", "data"); err != nil {
		t.Fatalf("dev should accept without secret, got: %v", err)
	}
}

func TestVerifier_StripeEmptySecretRejectsInProduction(t *testing.T) {
	v := NewWebhookVerifier("", "", true)
	if err := v.VerifyStripe([]byte("body"), "sig"); err == nil {
		t.Fatal("empty Stripe secret in production should reject")
	}
}
