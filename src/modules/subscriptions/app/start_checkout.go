package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// StartCheckoutInput is what the controller passes after auth + validation.
// SuccessURL / CancelURL are server-side defaults (PUBLIC_BASE_URL +
// "/settings/billing?…") — the FE doesn't get to override them, so a malicious
// redirect can't be smuggled in.
//
// PaymentMethod: "" o "card" = comportamiento default. "oxxo" sólo es válido
// con Provider=stripe y Plan=standard_annual (ver gateway.go para el por qué
// — Stripe no soporta OXXO en mode=subscription, así que sólo lo ofrecemos
// como pago one-time del plan anual).
type StartCheckoutInput struct {
	GymID         uuid.UUID
	UserID        uuid.UUID
	Provider      subDomain.Provider
	Plan          string
	PaymentMethod string
}

// StartCheckoutOutput is the redirect URL the FE opens in the browser.
type StartCheckoutOutput struct {
	URL       string
	SessionID string
}

// StartCheckout creates a Stripe Checkout Session (mode=subscription) or a
// Mercado Pago preapproval, depending on the provider the gym owner picked.
// The gateway abstraction means swapping a processor (or adding a third) is a
// new file in infraestructure/payments — this layer doesn't change.
type StartCheckout struct {
	Launch   subDomain.LaunchRepository // nil closes new admissions; existing benefits still apply.
	Gateways map[subDomain.Provider]subDomain.CheckoutGateway
	Gyms     gymRepo.GymRepository
	// Events dice cómo se pagó el plan vigente (ver runningSubscription).
	// nil solo en pruebas que no tocan planes de pago.
	Events     subDomain.EventRepository
	Users      userRepo.UserRepository
	UoW        sharedDomain.UnitOfWork
	SuccessURL string
	CancelURL  string
}

func NewStartCheckout(
	gateways map[subDomain.Provider]subDomain.CheckoutGateway,
	gyms gymRepo.GymRepository,
	events subDomain.EventRepository,
	users userRepo.UserRepository,
	uow sharedDomain.UnitOfWork,
	successURL, cancelURL string,
) *StartCheckout {
	return &StartCheckout{
		Gateways:   gateways,
		Gyms:       gyms,
		Events:     events,
		Users:      users,
		UoW:        uow,
		SuccessURL: successURL,
		CancelURL:  cancelURL,
	}
}

func (uc *StartCheckout) Execute(ctx context.Context, in StartCheckoutInput) (StartCheckoutOutput, error) {
	if in.GymID == uuid.Nil {
		return StartCheckoutOutput{}, sharedDomain.NewValidationError(errors.New("gym_id is required"))
	}
	if !gymDomain.IsPaidPlan(in.Plan) {
		return StartCheckoutOutput{}, sharedDomain.NewValidationError(errors.New("plan must be standard_monthly, standard_annual, plus_monthly or plus_annual"))
	}
	// Normaliza método de pago. Cliente puede mandar "" o "card" para
	// flujo default; "oxxo" sólo aplica al combo Stripe + Standard anual.
	switch in.PaymentMethod {
	case "", "card", "oxxo":
	default:
		return StartCheckoutOutput{}, sharedDomain.NewValidationError(errors.New("payment_method must be empty, card, or oxxo"))
	}
	if in.PaymentMethod == "oxxo" {
		if in.Provider != subDomain.ProviderStripe {
			return StartCheckoutOutput{}, sharedDomain.NewValidationError(errors.New("oxxo only available via stripe"))
		}

	}
	gateway, ok := uc.Gateways[in.Provider]
	if !ok || gateway == nil {
		return StartCheckoutOutput{}, sharedDomain.NewBusinessError(subDomain.ErrGatewayUnavailable, "Esta forma de pago no está disponible. Intenta con la otra.")
	}

	tx, err := uc.UoW.Query(ctx)
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		return StartCheckoutOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	gym, err := uc.Gyms.GetByID(tx, in.GymID)
	if err != nil {
		return StartCheckoutOutput{}, err
	}
	if err := uc.runningSubscription(tx, gym, time.Now().UTC()); err != nil {
		return StartCheckoutOutput{}, err
	}
	launchPricing := gym.AllFeaturesAccess
	if !launchPricing && uc.Launch != nil {
		remaining, err := uc.Launch.Remaining(tx)
		if err != nil {
			return StartCheckoutOutput{}, sharedDomain.NewUnexpectedError(err)
		}
		launchPricing = remaining > 0
	}
	if launchPricing {
		if in.Plan == gymDomain.PlanPlusMonthly {
			in.Plan = gymDomain.PlanStandardMonthly
		}
		if in.Plan == gymDomain.PlanPlusAnnual {
			in.Plan = gymDomain.PlanStandardAnnual
		}
	}
	if in.PaymentMethod == "oxxo" && in.Plan != gymDomain.PlanStandardAnnual {
		return StartCheckoutOutput{}, sharedDomain.NewValidationError(errors.New("oxxo only available for standard_annual"))
	}
	user, err := uc.Users.GetByID(tx, in.UserID)
	if err != nil {
		return StartCheckoutOutput{}, err
	}

	gymName := ""
	if gym.Name != nil {
		gymName = *gym.Name
	}
	sharedDomain.CloseQuery(tx)
	res, err := gateway.StartCheckout(ctx, subDomain.CheckoutRequest{
		LaunchPricing: launchPricing,
		GymID:         gym.ID,
		Plan:          in.Plan,
		GymName:       gymName,
		CustomerEmail: user.Email,
		SuccessURL:    uc.SuccessURL,
		CancelURL:     uc.CancelURL,
		PaymentMethod: in.PaymentMethod,
	})
	if err != nil {
		if errors.Is(err, subDomain.ErrUnsupportedPlan) {
			return StartCheckoutOutput{}, sharedDomain.NewBusinessError(err, "Este plan aún no está habilitado en este método de pago.")
		}
		return StartCheckoutOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	return StartCheckoutOutput{URL: res.URL, SessionID: res.SessionID}, nil
}

// runningSubscription evita el cobro doble: Stripe crea una suscripción nueva
// por cada checkout aunque la anterior siga cobrando. Bloquea cuando el plan
// vigente se cobra solo (tarjeta o Mercado Pago):
//   - past_due: Stripe reintenta el cobro; se arregla cambiando la tarjeta;
//   - activo: para cambiar de plan o de tarjeta está el portal de pagos;
//   - cancelado con periodo pagado por delante: se reactiva en el portal.
//
// Deja pasar la prueba, un plan ya terminado y los periodos pagados por OXXO
// o a mano, que se renuevan justo con un checkout nuevo (el recordatorio
// anual de OXXO usa este mismo caso de uso).
func (uc *StartCheckout) runningSubscription(tx sharedDomain.Transaction, gym *gymDomain.Gym, now time.Time) error {
	if gym.SubscriptionPlan == gymDomain.PlanTrial {
		return nil
	}
	switch gym.SubscriptionStatus {
	case gymDomain.StatusPastDue:
		return sharedDomain.NewBusinessError(subDomain.ErrSubscriptionPastDue, "")
	case gymDomain.StatusCancelled:
		if gym.SubscriptionEndsAt == nil || !gym.SubscriptionEndsAt.After(now) {
			return nil
		}
	}
	last, err := uc.lastPayment(tx, gym.ID)
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	if !last.RenewsAutomatically() {
		return nil
	}
	if gym.SubscriptionStatus == gymDomain.StatusCancelled {
		return sharedDomain.NewBusinessError(subDomain.ErrSubscriptionStillRunning, "")
	}
	return sharedDomain.NewBusinessError(subDomain.ErrSubscriptionRenewsItself, "")
}

// lastPayment es el evento activated/renewed más reciente del gym, o nil.
func (uc *StartCheckout) lastPayment(tx sharedDomain.Transaction, gymID uuid.UUID) (*subDomain.Event, error) {
	if uc.Events == nil {
		return nil, nil
	}
	events, err := uc.Events.ListByGym(tx, gymID, 50)
	if err != nil {
		return nil, err
	}
	var last *subDomain.Event
	for _, e := range events {
		if (e.Type == subDomain.EventActivated || e.Type == subDomain.EventRenewed) && (last == nil || e.OccurredAt.After(last.OccurredAt)) {
			last = e
		}
	}
	return last, nil
}
