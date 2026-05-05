package app

import (
	"context"
	"errors"

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
type StartCheckoutInput struct {
	GymID    uuid.UUID
	UserID   uuid.UUID
	Provider subDomain.Provider
	Plan     string
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
	Gateways   map[subDomain.Provider]subDomain.CheckoutGateway
	Gyms       gymRepo.GymRepository
	Users      userRepo.UserRepository
	UoW        sharedDomain.UnitOfWork
	SuccessURL string
	CancelURL  string
}

func NewStartCheckout(
	gateways map[subDomain.Provider]subDomain.CheckoutGateway,
	gyms gymRepo.GymRepository,
	users userRepo.UserRepository,
	uow sharedDomain.UnitOfWork,
	successURL, cancelURL string,
) *StartCheckout {
	return &StartCheckout{
		Gateways:   gateways,
		Gyms:       gyms,
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
	if in.Plan != gymDomain.PlanProMonthly && in.Plan != gymDomain.PlanProAnnual {
		return StartCheckoutOutput{}, sharedDomain.NewValidationError(errors.New("plan must be pro_monthly or pro_annual"))
	}
	gateway, ok := uc.Gateways[in.Provider]
	if !ok || gateway == nil {
		return StartCheckoutOutput{}, sharedDomain.NewBusinessError(subDomain.ErrGatewayUnavailable, "Esta forma de pago no está disponible. Intenta con la otra.")
	}

	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return StartCheckoutOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	gym, err := uc.Gyms.GetByID(tx, in.GymID)
	if err != nil {
		return StartCheckoutOutput{}, err
	}
	user, err := uc.Users.GetByID(tx, in.UserID)
	if err != nil {
		return StartCheckoutOutput{}, err
	}

	gymName := ""
	if gym.Name != nil {
		gymName = *gym.Name
	}
	res, err := gateway.StartCheckout(ctx, subDomain.CheckoutRequest{
		GymID:         gym.ID,
		Plan:          in.Plan,
		GymName:       gymName,
		CustomerEmail: user.Email,
		SuccessURL:    uc.SuccessURL,
		CancelURL:     uc.CancelURL,
	})
	if err != nil {
		if errors.Is(err, subDomain.ErrUnsupportedPlan) {
			return StartCheckoutOutput{}, sharedDomain.NewBusinessError(err, "Este plan aún no está habilitado en este método de pago.")
		}
		return StartCheckoutOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	return StartCheckoutOutput{URL: res.URL, SessionID: res.SessionID}, nil
}
