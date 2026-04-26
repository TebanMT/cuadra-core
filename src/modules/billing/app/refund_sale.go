package app

import (
	"context"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RefundSaleInput backs UC-026.
//
// MVP NOTE: per the Sesión 4 brief ("Devolución de venta — P2, manual con UC-022"),
// the dedicated sale-refund path is intentionally a thin wrapper that resolves
// the parent Payment from the Sale id and delegates to UC-022 (RefundPayment)
// — the operator may then call UC-024 (AdjustStock 'restock') manually if they
// want stock back. We keep this seam so the interface is in place for V1.0
// when ops feedback may justify a richer flow that auto-restocks each line.
type RefundSaleInput struct {
	GymID        uuid.UUID
	ActorUserID  uuid.UUID
	SaleID       uuid.UUID
	Reason       string
	Method       string
	RestoreStock bool // when true, IncrementForRefund per item (V1.0+)
}

type RefundSaleOutput struct {
	RefundID uuid.UUID
	Amount   float64
	Restored bool
}

type RefundSale struct {
	Sales  billingRepo.SaleRepository
	Refund *RefundPayment
	UoW    sharedDomain.UnitOfWork
}

func NewRefundSale(sales billingRepo.SaleRepository, refund *RefundPayment, uow sharedDomain.UnitOfWork) *RefundSale {
	return &RefundSale{Sales: sales, Refund: refund, UoW: uow}
}

func (uc *RefundSale) Execute(ctx context.Context, in RefundSaleInput) (*RefundSaleOutput, error) {
	// MVP path: read Sale → take its payment_id → call UC-022. We do NOT
	// auto-restore stock yet (DA-26 deferred). RestoreStock is reserved for V1.0.
	tx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	s, err := uc.Sales.GetByID(tx, in.SaleID)
	if err != nil {
		return nil, err
	}
	if s.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrCrossGym, "")
	}
	out, err := uc.Refund.Execute(ctx, RefundPaymentInput{
		GymID:           in.GymID,
		ActorUserID:     in.ActorUserID,
		ParentPaymentID: s.PaymentID,
		Reason:          in.Reason,
		Method:          in.Method,
	})
	if err != nil {
		return nil, err
	}
	return &RefundSaleOutput{
		RefundID: out.RefundID,
		Amount:   out.Amount,
		Restored: false,
	}, nil
}
