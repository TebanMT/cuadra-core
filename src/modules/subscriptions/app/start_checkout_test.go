package app

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Tests for StartCheckout — el use case que decide qué gateway invocar y
// valida el método de pago. Cubre la matriz mínima de PaymentMethod tras
// añadir OXXO (Opción A: anual + Stripe únicamente).

// ── Fakes (locales — el fakeUoW + fakeGyms ya viven en record_event_test.go) ─

type fakeUsers struct {
	byID  map[uuid.UUID]*userDomain.User
	byGym map[uuid.UUID][]*userDomain.User
}

func (r *fakeUsers) Create(_ sharedDomain.Transaction, u *userDomain.User) (*userDomain.User, error) {
	r.byID[u.ID] = u
	return u, nil
}
func (r *fakeUsers) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*userDomain.User, error) {
	if u, ok := r.byID[id]; ok {
		return u, nil
	}
	return nil, errors.New("user not found")
}
func (r *fakeUsers) GetByEmail(_ sharedDomain.Transaction, _ string) (*userDomain.User, error) {
	return nil, errors.New("not implemented")
}
func (r *fakeUsers) ExistsByEmail(_ sharedDomain.Transaction, _ string) (bool, error) {
	return false, nil
}
func (r *fakeUsers) ExistsByPhoneInGym(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _ *uuid.UUID) (bool, error) {
	return false, nil
}
func (r *fakeUsers) Update(_ sharedDomain.Transaction, u *userDomain.User) (*userDomain.User, error) {
	r.byID[u.ID] = u
	return u, nil
}
func (r *fakeUsers) ListByGym(_ sharedDomain.Transaction, gymID uuid.UUID) ([]*userDomain.User, error) {
	return r.byGym[gymID], nil
}
func (r *fakeUsers) CountOperatorsByGym(_ sharedDomain.Transaction, _ uuid.UUID) (int, error) {
	return 0, nil
}

var _ userRepo.UserRepository = (*fakeUsers)(nil)

// recordingGateway captura el último CheckoutRequest para asertar que el
// método de pago llega al gateway. Devuelve una URL fija; nunca llama a
// la red.
type recordingGateway struct {
	provider subDomain.Provider
	last     *subDomain.CheckoutRequest
}

func (g *recordingGateway) Provider() subDomain.Provider { return g.provider }
func (g *recordingGateway) StartCheckout(_ context.Context, in subDomain.CheckoutRequest) (subDomain.CheckoutResult, error) {
	g.last = &in
	return subDomain.CheckoutResult{URL: "https://example/test", SessionID: "sess_test"}, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────

func newStartCheckoutFixture(t *testing.T) (*StartCheckout, *recordingGateway, uuid.UUID, uuid.UUID) {
	t.Helper()
	gymID := uuid.New()
	userID := uuid.New()
	gyms := &fakeGyms{byID: map[uuid.UUID]*gymDomain.Gym{
		gymID: {ID: gymID, SubscriptionPlan: gymDomain.PlanTrial, SubscriptionStatus: gymDomain.StatusActive},
	}}
	users := &fakeUsers{byID: map[uuid.UUID]*userDomain.User{
		userID: {ID: userID, Email: "test@example.com"},
	}}
	stripe := &recordingGateway{provider: subDomain.ProviderStripe}
	mp := &recordingGateway{provider: subDomain.ProviderMercadoPago}
	uc := &StartCheckout{
		Gateways: map[subDomain.Provider]subDomain.CheckoutGateway{
			subDomain.ProviderStripe:      stripe,
			subDomain.ProviderMercadoPago: mp,
		},
		Gyms:       gyms,
		Users:      users,
		UoW:        fakeUoW{},
		SuccessURL: "https://app/success",
		CancelURL:  "https://app/cancel",
	}
	return uc, stripe, gymID, userID
}

// ── Tests ────────────────────────────────────────────────────────────────

func TestStartCheckout_DefaultPaymentMethodIsCard(t *testing.T) {
	// PaymentMethod="" debe llegar tal cual al gateway (el gateway decide
	// el default — hoy Stripe Subscriptions con tarjeta). No queremos
	// que el use case "rellene" "card" por su cuenta para no esconder
	// bugs si más adelante algún gateway distingue "" vs "card".
	uc, stripe, gymID, userID := newStartCheckoutFixture(t)
	out, err := uc.Execute(context.Background(), StartCheckoutInput{
		GymID: gymID, UserID: userID,
		Provider: subDomain.ProviderStripe,
		Plan:     gymDomain.PlanStandardMonthly,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.URL == "" {
		t.Fatalf("expected URL, got empty")
	}
	if stripe.last == nil {
		t.Fatal("gateway not invoked")
	}
	if stripe.last.PaymentMethod != "" {
		t.Errorf("payment_method passthrough = %q, want \"\"", stripe.last.PaymentMethod)
	}
}

func TestStartCheckout_CardPaymentMethodIsValid(t *testing.T) {
	uc, stripe, gymID, userID := newStartCheckoutFixture(t)
	_, err := uc.Execute(context.Background(), StartCheckoutInput{
		GymID: gymID, UserID: userID,
		Provider:      subDomain.ProviderStripe,
		Plan:          gymDomain.PlanStandardAnnual,
		PaymentMethod: "card",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stripe.last.PaymentMethod != "card" {
		t.Errorf("payment_method passthrough = %q, want card", stripe.last.PaymentMethod)
	}
}

func TestStartCheckout_OXXOValidForStripeAnnual(t *testing.T) {
	uc, stripe, gymID, userID := newStartCheckoutFixture(t)
	_, err := uc.Execute(context.Background(), StartCheckoutInput{
		GymID: gymID, UserID: userID,
		Provider:      subDomain.ProviderStripe,
		Plan:          gymDomain.PlanStandardAnnual,
		PaymentMethod: "oxxo",
	})
	if err != nil {
		t.Fatalf("oxxo+stripe+annual should be valid, got: %v", err)
	}
	if stripe.last.PaymentMethod != "oxxo" {
		t.Errorf("payment_method=%q, want oxxo", stripe.last.PaymentMethod)
	}
	if stripe.last.Plan != gymDomain.PlanStandardAnnual {
		t.Errorf("plan=%q, want standard_annual", stripe.last.Plan)
	}
}

func TestStartCheckout_OXXORejectsMonthly(t *testing.T) {
	// OXXO sólo aplica al anual. El mensual con OXXO debe fallar antes de
	// invocar el gateway — el cron de re-emisión no está construido,
	// prometerlo en mensual es prometer algo que rompe.
	uc, stripe, gymID, userID := newStartCheckoutFixture(t)
	_, err := uc.Execute(context.Background(), StartCheckoutInput{
		GymID: gymID, UserID: userID,
		Provider:      subDomain.ProviderStripe,
		Plan:          gymDomain.PlanStandardMonthly,
		PaymentMethod: "oxxo",
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if stripe.last != nil {
		t.Fatal("gateway should not be invoked after validation failure")
	}
}

func TestStartCheckout_OXXORejectsMercadoPago(t *testing.T) {
	// MP no expone OXXO via su API; sólo Stripe MX lo hace. La validación
	// vive en el use case para que el FE pueda mostrar el error coherente
	// sin que el gateway llegue siquiera a recibir el request.
	uc, _, gymID, userID := newStartCheckoutFixture(t)
	_, err := uc.Execute(context.Background(), StartCheckoutInput{
		GymID: gymID, UserID: userID,
		Provider:      subDomain.ProviderMercadoPago,
		Plan:          gymDomain.PlanStandardAnnual,
		PaymentMethod: "oxxo",
	})
	if err == nil {
		t.Fatal("expected validation error for oxxo+mercadopago, got nil")
	}
}

func TestStartCheckout_RejectsUnknownPaymentMethod(t *testing.T) {
	uc, _, gymID, userID := newStartCheckoutFixture(t)
	_, err := uc.Execute(context.Background(), StartCheckoutInput{
		GymID: gymID, UserID: userID,
		Provider:      subDomain.ProviderStripe,
		Plan:          gymDomain.PlanStandardAnnual,
		PaymentMethod: "transfer",
	})
	if err == nil {
		t.Fatal("expected validation error for unknown payment_method, got nil")
	}
}

func TestStartCheckout_RejectsInvalidPlan(t *testing.T) {
	// Sanity: planes desconocidos siguen rechazados aún con payment_method
	// válido. El gate IsPaidPlan ataja antes de la validación de método.
	uc, _, gymID, userID := newStartCheckoutFixture(t)
	_, err := uc.Execute(context.Background(), StartCheckoutInput{
		GymID: gymID, UserID: userID,
		Provider:      subDomain.ProviderStripe,
		Plan:          "garbage_plan",
		PaymentMethod: "card",
	})
	if err == nil {
		t.Fatal("expected validation error for invalid plan")
	}
}
