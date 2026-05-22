package app

import (
	"context"
	"errors"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// StartBillingPortalInput is what the controller passes after auth. The
// ReturnURL is computed server-side from PUBLIC_BASE_URL — the FE doesn't get
// to override it (same posture as StartCheckout).
type StartBillingPortalInput struct {
	GymID uuid.UUID
}

// StartBillingPortalOutput is the URL the FE redirects the browser to.
type StartBillingPortalOutput struct {
	URL string
}

// ErrNoStripeCustomer — el gym todavía no ha pasado un primer cobro por
// Stripe (sin webhook que haya traído customer_id). Hasta entonces no hay
// nada que mostrar en el portal. El controller lo mapea a 409 con copy
// "todavía no tienes método de pago activo".
var ErrNoStripeCustomer = errors.New("no stripe customer id on file for this gym")

// StartBillingPortal abre una sesión del Stripe Customer Portal hosted. Es
// la única manera que tiene el dueño de:
//   - actualizar tarjeta sin re-onboarding,
//   - descargar facturas / recibos,
//   - cambiar de Standard a Plus (o viceversa) sin tocar nada en el dashboard,
//   - cancelar la suscripción.
//
// Stripe maneja toda la UI; nosotros recibimos las consecuencias por webhook
// (RecordEvent las aplica al gym).
type StartBillingPortal struct {
	Stripe    subDomain.BillingPortalGateway
	Gyms      gymRepo.GymRepository
	UoW       sharedDomain.UnitOfWork
	ReturnURL string
}

func NewStartBillingPortal(
	stripe subDomain.BillingPortalGateway,
	gyms gymRepo.GymRepository,
	uow sharedDomain.UnitOfWork,
	returnURL string,
) *StartBillingPortal {
	return &StartBillingPortal{
		Stripe:    stripe,
		Gyms:      gyms,
		UoW:       uow,
		ReturnURL: returnURL,
	}
}

func (uc *StartBillingPortal) Execute(ctx context.Context, in StartBillingPortalInput) (StartBillingPortalOutput, error) {
	if in.GymID == uuid.Nil {
		return StartBillingPortalOutput{}, sharedDomain.NewValidationError(errors.New("gym_id is required"))
	}
	if uc.Stripe == nil {
		return StartBillingPortalOutput{}, sharedDomain.NewBusinessError(subDomain.ErrGatewayUnavailable, "El portal de facturación no está disponible. Intenta más tarde.")
	}

	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return StartBillingPortalOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	gym, err := uc.Gyms.GetByID(tx, in.GymID)
	if err != nil {
		return StartBillingPortalOutput{}, err
	}
	if gym.StripeCustomerID == nil || *gym.StripeCustomerID == "" {
		return StartBillingPortalOutput{}, sharedDomain.NewBusinessError(ErrNoStripeCustomer, "Todavía no tienes método de pago activo. Activa tu plan primero.")
	}

	res, err := uc.Stripe.StartBillingPortal(ctx, subDomain.BillingPortalRequest{
		CustomerID: *gym.StripeCustomerID,
		ReturnURL:  uc.ReturnURL,
	})
	if err != nil {
		return StartBillingPortalOutput{}, sharedDomain.NewUnexpectedError(err)
	}
	return StartBillingPortalOutput{URL: res.URL}, nil
}
