package app

import (
	"context"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	refundDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/refund"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"time"
)

// RefundSaleInput backs UC-026. The sale-specific path resolves the aggregate
// root and delegates the atomic money, debt, selected-line and stock effects to
// RefundPayment. Each returned line must state its physical disposition.
type RefundSaleInput struct {
	GymID          uuid.UUID
	ActorUserID    uuid.UUID
	SaleID         uuid.UUID
	Reason         string
	Method         string
	CashDrawerID   *uuid.UUID
	Amount         float64
	PaymentDate    time.Time
	IdempotencyKey string
	Items          []refundDomain.ItemInput
}

type RefundSaleOutput struct {
	RefundID         uuid.UUID
	Amount           float64
	BalanceCancelled float64
	Restored         bool
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
	// Resolve Sale → root Payment, then execute the complete refund command.
	// Stock returns only for items explicitly marked returned_to_stock.
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
		CashDrawerID:    in.CashDrawerID,
		Amount:          in.Amount,
		PaymentDate:     in.PaymentDate,
		IdempotencyKey:  in.IdempotencyKey,
		SaleID:          &s.ID,
		Items:           in.Items,
	})
	if err != nil {
		return nil, err
	}
	restored := false
	for _, item := range in.Items {
		if item.Quantity > 0 && item.Disposition == refundDomain.ReturnedToStock {
			restored = true
			break
		}
	}
	return &RefundSaleOutput{
		RefundID:         out.RefundID,
		Amount:           out.Amount,
		BalanceCancelled: out.BalanceCancelled,
		Restored:         restored,
	}, nil
}
