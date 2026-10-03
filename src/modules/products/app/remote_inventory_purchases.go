package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchase "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	repo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CreateRemoteInventoryPurchaseInput struct {
	ID, GymID, ProductID, ActorUserID uuid.UUID
	ActorRole                         string
	Quantity                          int
	UnitCost                          float64
	PaidOn                            time.Time
	PaymentMethod, PaidFrom           string
}
type CreateRemoteInventoryPurchase struct {
	Receipts  repo.InventoryPurchaseReceiptRepository
	Purchases repo.InventoryPurchaseRepository
	Products  repo.ProductRepository
	Gyms      gymRepo.GymRepository
	UoW       shared.UnitOfWork
	Audit     audit.Recorder
}

func NewCreateRemoteInventoryPurchase(purchases repo.InventoryPurchaseRepository, products repo.ProductRepository, gyms gymRepo.GymRepository, uow shared.UnitOfWork, recorder audit.Recorder) *CreateRemoteInventoryPurchase {
	return &CreateRemoteInventoryPurchase{Purchases: purchases, Products: products, Gyms: gyms, UoW: uow, Audit: recorder}
}
func (uc *CreateRemoteInventoryPurchase) Execute(ctx context.Context, in CreateRemoteInventoryPurchaseInput) (*InventoryPurchaseView, error) {
	if in.ActorRole != "owner" {
		return nil, shared.NewBusinessError(prodErrors.ErrPurchaseOwnerRequired, "")
	}
	// Remote payments cannot claim a withdrawal from the reception drawer.
	if in.PaidFrom != purchase.PaidFromGymFund && in.PaidFrom != purchase.PaidFromExternal {
		return nil, shared.NewValidationError(prodErrors.ErrInvalidPurchaseSource)
	}
	now := time.Now().UTC()
	var out *InventoryPurchaseView
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		day, err := resolvePurchasePaidOn(tx, uc.Gyms, in.GymID, in.PaidOn, now)
		if err != nil {
			return err
		}
		p, err := purchase.New(purchase.Input{ID: in.ID, GymID: in.GymID, ProductID: in.ProductID, CreatedBy: in.ActorUserID, AwaitingReceipt: true, Quantity: in.Quantity, UnitCost: &in.UnitCost, Status: purchase.StatusPaid, PaidOn: &day, PaymentMethod: strings.TrimSpace(in.PaymentMethod), PaidFrom: in.PaidFrom, IdempotencyKey: "remote-purchase:" + in.ID.String(), Now: now})
		if err != nil {
			return shared.NewValidationError(err)
		}
		product, err := uc.Products.GetByID(tx, in.ProductID)
		if err != nil {
			return err
		}
		if product.GymID != in.GymID {
			return shared.NewBusinessError(prodErrors.ErrCrossGym, "")
		}
		existing, err := uc.Purchases.GetByIdempotencyKey(tx, in.GymID, p.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			// Reusing a submitted ID with different facts is never a second payment.
			if !existing.HasSeparateReceipt() || existing.ProductID != p.ProductID || existing.Quantity != p.Quantity || existing.UnitCost == nil || *existing.UnitCost != *p.UnitCost || existing.PaidOn == nil || !existing.PaidOn.Equal(*p.PaidOn) || existing.PaymentMethod == nil || *existing.PaymentMethod != *p.PaymentMethod || existing.PaidFrom == nil || *existing.PaidFrom != *p.PaidFrom || !existing.IsPaid() {
				return shared.NewBusinessError(prodErrors.ErrAdjustmentIdempotencyConflict, "")
			}
			out = &InventoryPurchaseView{Purchase: existing, ProductName: product.Name}
			if uc.Receipts != nil {
				out.Receipt, err = uc.Receipts.GetByPurchase(tx, in.GymID, existing.ID)
				if err != nil {
					return err
				}
			}
			return nil
		}
		if !product.Active {
			return shared.NewValidationError(prodErrors.ErrPurchaseReceiptInvalid)
		}
		if _, err = uc.Purchases.Create(tx, p); err != nil {
			return err
		}
		if err = recordInventoryPurchaseAudit(ctx, uc.Audit, tx, p, in.ActorUserID, "create_remote", map[string]any{"quantity": p.Quantity, "total_amount": p.TotalAmount, "paid_from": p.PaidFrom, "payment_method": p.PaymentMethod}, now); err != nil {
			return err
		}
		out = &InventoryPurchaseView{Purchase: p, ProductName: product.Name}
		return nil
	})
	return out, err
}

type ReceiveInventoryPurchaseInput struct {
	GymID, PurchaseID, ActorUserID uuid.UUID
	ActorRole                      string
	Quantity                       int
}
type ReceiveInventoryPurchase struct {
	Purchases repo.InventoryPurchaseRepository
	Receipts  repo.InventoryPurchaseReceiptRepository
	UoW       shared.UnitOfWork
	Audit     audit.Recorder
}

func NewReceiveInventoryPurchase(purchases repo.InventoryPurchaseRepository, receipts repo.InventoryPurchaseReceiptRepository, uow shared.UnitOfWork, recorder audit.Recorder) *ReceiveInventoryPurchase {
	return &ReceiveInventoryPurchase{purchases, receipts, uow, recorder}
}
func (uc *ReceiveInventoryPurchase) Execute(ctx context.Context, in ReceiveInventoryPurchaseInput) (*purchase.Receipt, error) {
	if in.ActorRole != "owner" && in.ActorRole != "operator" {
		return nil, shared.NewBusinessError(prodErrors.ErrPurchaseReceiptInvalid, "")
	}
	var out *purchase.Receipt
	err := uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		existing, err := uc.Receipts.GetByPurchase(tx, in.GymID, in.PurchaseID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Quantity != in.Quantity {
				return shared.NewBusinessError(prodErrors.ErrPurchaseReceiptConflict, "")
			}
			out = existing
			return nil
		}
		p, err := uc.Purchases.GetByID(tx, in.GymID, in.PurchaseID)
		if err != nil {
			return err
		}
		receipt, err := purchase.NewReceipt(p, in.ActorUserID, in.Quantity, time.Now().UTC())
		if err != nil {
			return shared.NewValidationError(err)
		}
		out, err = uc.Receipts.Create(tx, receipt)
		if err != nil {
			return err
		}
		return recordInventoryPurchaseAudit(ctx, uc.Audit, tx, p, in.ActorUserID, "receive", map[string]any{"receipt_id": out.ID, "quantity": out.Quantity, "stock_movement_id": out.StockMovementID()}, out.CreatedAt)
	})
	return out, err
}

func (uc *CreateRemoteInventoryPurchase) WithReceipts(r repo.InventoryPurchaseReceiptRepository) *CreateRemoteInventoryPurchase {
	uc.Receipts = r
	return uc
}
