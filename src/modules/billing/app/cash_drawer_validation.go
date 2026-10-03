package app

import (
	"github.com/google/uuid"

	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// CashDrawerValidator is the narrow capability physical-money commands need
// from the drawer catalog. Validation runs inside the command transaction,
// after an idempotent replay check and before any side effect.
type CashDrawerValidator interface {
	ValidateActiveCashDrawer(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID) error
}

func validateRequestedCashDrawer(validator CashDrawerValidator, tx sharedDomain.Transaction,
	gymID uuid.UUID, method string, drawerID *uuid.UUID) error {
	if method != paymentDomain.MethodCash || drawerID == nil || *drawerID == uuid.Nil || validator == nil {
		return nil
	}
	return validator.ValidateActiveCashDrawer(tx, gymID, *drawerID)
}
