package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
)

// paidGymCheckout arma un checkout para un gym con plan de pago y su historial
// de pagos. Devuelve el gateway para saber si se llegó a crear la sesión.
func paidGymCheckout(t *testing.T, status string, endsAt time.Time, payments ...*subDomain.Event) (*StartCheckout, *recordingGateway, uuid.UUID, uuid.UUID) {
	t.Helper()
	gymID, userID := uuid.New(), uuid.New()
	gyms := &fakeGyms{byID: map[uuid.UUID]*gymDomain.Gym{gymID: {
		ID: gymID, SubscriptionPlan: gymDomain.PlanStandardAnnual, SubscriptionStatus: status, SubscriptionEndsAt: &endsAt,
	}}}
	events := &fakeEvents{}
	for _, p := range payments {
		p.GymID = gymID
		events.rows = append(events.rows, p)
	}
	stripe := &recordingGateway{provider: subDomain.ProviderStripe}
	uc := NewStartCheckout(
		map[subDomain.Provider]subDomain.CheckoutGateway{subDomain.ProviderStripe: stripe},
		gyms, events,
		&fakeUsers{byID: map[uuid.UUID]*userDomain.User{userID: {ID: userID, Email: "owner@example.com"}}},
		fakeUoW{}, "https://app/success", "https://app/cancel",
	)
	return uc, stripe, gymID, userID
}

func payment(provider subDomain.Provider, typ subDomain.EventType, method string, daysAgo int) *subDomain.Event {
	e := &subDomain.Event{Provider: provider, Type: typ, OccurredAt: time.Now().UTC().Add(-time.Duration(daysAgo) * 24 * time.Hour)}
	if method != "" {
		e.RawPayload = map[string]any{"payment_method": method}
	}
	return e
}

func startAnnual(uc *StartCheckout, gymID, userID uuid.UUID, method string) error {
	_, err := uc.Execute(context.Background(), StartCheckoutInput{
		GymID: gymID, UserID: userID, Provider: subDomain.ProviderStripe,
		Plan: gymDomain.PlanStandardAnnual, PaymentMethod: method,
	})
	return err
}

// Un plan que se cobra solo (tarjeta) no puede contratarse otra vez: Stripe
// crearía una segunda suscripción y el dueño pagaría doble.
func TestStartCheckout_BlocksSecondSubscription(t *testing.T) {
	future := time.Now().UTC().Add(20 * 24 * time.Hour)
	card := payment(subDomain.ProviderStripe, subDomain.EventActivated, "", 10)
	cases := []struct {
		name   string
		status string
		want   error
	}{
		{"plan activo con tarjeta", gymDomain.StatusActive, subDomain.ErrSubscriptionRenewsItself},
		{"cobro fallido", gymDomain.StatusPastDue, subDomain.ErrSubscriptionPastDue},
		{"cancelado con periodo pagado por delante", gymDomain.StatusCancelled, subDomain.ErrSubscriptionStillRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uc, stripe, gymID, userID := paidGymCheckout(t, tc.status, future, card)
			err := startAnnual(uc, gymID, userID, "card")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if stripe.last != nil {
				t.Fatal("no debe crearse una sesión de pago")
			}
			// El dueño ve el mensaje en español, sin prefijo técnico.
			if err.Error() != tc.want.Error() {
				t.Fatalf("message = %q, want %q", err.Error(), tc.want.Error())
			}
		})
	}
}

// Mercado Pago también cobra solo (preapproval).
func TestStartCheckout_BlocksWhileMercadoPagoRenews(t *testing.T) {
	future := time.Now().UTC().Add(20 * 24 * time.Hour)
	uc, _, gymID, userID := paidGymCheckout(t, gymDomain.StatusActive, future,
		payment(subDomain.ProviderMercadoPago, subDomain.EventRenewed, "", 3))
	if err := startAnnual(uc, gymID, userID, "card"); !errors.Is(err, subDomain.ErrSubscriptionRenewsItself) {
		t.Fatalf("err = %v, want ErrSubscriptionRenewsItself", err)
	}
}

// Manda el pago más reciente: si después de un OXXO pasó a tarjeta, ya se
// cobra solo.
func TestStartCheckout_UsesTheLatestPayment(t *testing.T) {
	future := time.Now().UTC().Add(200 * 24 * time.Hour)
	uc, _, gymID, userID := paidGymCheckout(t, gymDomain.StatusActive, future,
		payment(subDomain.ProviderStripe, subDomain.EventRenewed, "", 2),
		payment(subDomain.ProviderStripe, subDomain.EventActivated, "oxxo", 300))
	if err := startAnnual(uc, gymID, userID, "card"); !errors.Is(err, subDomain.ErrSubscriptionRenewsItself) {
		t.Fatalf("err = %v, want ErrSubscriptionRenewsItself", err)
	}
}

// Lo que se paga por periodo (OXXO, transferencia) se renueva justo con un
// checkout nuevo; un plan ya terminado vuelve a contratarse.
func TestStartCheckout_AllowsRenewalsAndEndedPlans(t *testing.T) {
	future := time.Now().UTC().Add(10 * 24 * time.Hour)
	past := time.Now().UTC().Add(-2 * 24 * time.Hour)
	cases := []struct {
		name    string
		status  string
		endsAt  time.Time
		payment *subDomain.Event
		method  string
	}{
		{"renovación anual por OXXO", gymDomain.StatusActive, future, payment(subDomain.ProviderStripe, subDomain.EventActivated, "oxxo", 355), "oxxo"},
		{"pagado por transferencia", gymDomain.StatusActive, future, payment(subDomain.ProviderManual, subDomain.EventActivated, "", 20), "card"},
		{"plan con tarjeta ya terminado", gymDomain.StatusCancelled, past, payment(subDomain.ProviderStripe, subDomain.EventActivated, "", 40), "card"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uc, stripe, gymID, userID := paidGymCheckout(t, tc.status, tc.endsAt, tc.payment)
			if err := startAnnual(uc, gymID, userID, tc.method); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if stripe.last == nil {
				t.Fatal("debe crearse la sesión de pago")
			}
		})
	}
}

func TestEventRenewsAutomatically(t *testing.T) {
	cases := []struct {
		name string
		e    *subDomain.Event
		want bool
	}{
		{"sin pago", nil, false},
		{"tarjeta Stripe", payment(subDomain.ProviderStripe, subDomain.EventRenewed, "", 0), true},
		{"OXXO", payment(subDomain.ProviderStripe, subDomain.EventActivated, "oxxo", 0), false},
		{"Mercado Pago", payment(subDomain.ProviderMercadoPago, subDomain.EventRenewed, "", 0), true},
		{"manual", payment(subDomain.ProviderManual, subDomain.EventActivated, "", 0), false},
		{"cancelación no es pago", payment(subDomain.ProviderStripe, subDomain.EventCancelled, "", 0), false},
	}
	for _, tc := range cases {
		if got := tc.e.RenewsAutomatically(); got != tc.want {
			t.Errorf("%s: RenewsAutomatically = %v, want %v", tc.name, got, tc.want)
		}
	}
}
