package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchaseDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// AdjustStockInput backs UC-024. Three movement types:
//
//	'restock'          -> stock += quantity, optional cost
//	'shrinkage'        -> stock -= quantity (rejected if would underflow)
//	'count_correction' -> stock = quantity (replaces, NOT adds)
//
// `Reason` is optional but encouraged.
type AdjustStockInput struct {
	GymID        uuid.UUID
	ActorUserID  uuid.UUID
	ActorRole    string
	ProductID    uuid.UUID
	MovementType string
	Quantity     int      // restock/shrinkage = delta size; count_correction = absolute
	Cost         *float64 // optional, only meaningful for restock
	// IsPurchase=true turns the restock into an explicit financial purchase.
	// nil/false is a physical inventory capture and never enters outflows.
	IsPurchase     *bool
	PurchaseStatus string
	PaidOn         *time.Time
	PaymentMethod  string
	PaidFrom       string
	CashDrawerID   *uuid.UUID
	IdempotencyKey string
	Reason         *string
}

type AdjustStockOutput struct {
	NewStock       int        `json:"new_stock"`
	Delta          int        `json:"delta"`
	MovementID     uuid.UUID  `json:"movement_id"`
	PurchaseID     *uuid.UUID `json:"purchase_id,omitempty"`
	CashMovementID *uuid.UUID `json:"cash_movement_id,omitempty"`
}

// InventoryPurchaseCashRecorder is implemented by expenses/app. The narrow
// primitive seam keeps the products domain independent from Expense types.
type InventoryPurchaseCashRecorder interface {
	RecordInventoryPurchase(context.Context, sharedDomain.Transaction,
		uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, time.Time, float64, string, time.Time) (uuid.UUID, error)
	UseExistingInventoryPurchaseCash(context.Context, sharedDomain.Transaction,
		uuid.UUID, uuid.UUID, uuid.UUID, *uuid.UUID, time.Time, float64, time.Time) (uuid.UUID, error)
	InventoryPurchaseUsesCashDrawer(sharedDomain.Transaction, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
	CashDrawerForInventoryPurchase(sharedDomain.Transaction, uuid.UUID, uuid.UUID) (*uuid.UUID, error)
	VoidInventoryPurchaseCash(context.Context, sharedDomain.Transaction, uuid.UUID, uuid.UUID, uuid.UUID, string, time.Time) error
}

type AdjustStock struct {
	Products       prodRepo.ProductRepository
	StockMovements prodRepo.StockMovementRepository
	Purchases      prodRepo.InventoryPurchaseRepository
	PurchaseCash   InventoryPurchaseCashRecorder
	Gyms           gymRepo.GymRepository
	UoW            sharedDomain.UnitOfWork
	Audit          audit.Recorder
}

func NewAdjustStock(products prodRepo.ProductRepository, movements prodRepo.StockMovementRepository,
	uow sharedDomain.UnitOfWork, recorder audit.Recorder) *AdjustStock {
	return &AdjustStock{Products: products, StockMovements: movements, UoW: uow, Audit: recorder}
}

func (uc *AdjustStock) WithPurchases(purchases prodRepo.InventoryPurchaseRepository, cash InventoryPurchaseCashRecorder) *AdjustStock {
	uc.Purchases = purchases
	uc.PurchaseCash = cash
	return uc
}

func (uc *AdjustStock) WithGyms(gyms gymRepo.GymRepository) *AdjustStock {
	uc.Gyms = gyms
	return uc
}

func (uc *AdjustStock) Execute(ctx context.Context, in AdjustStockInput) (*AdjustStockOutput, error) {
	if !stockMovementDomain.ValidType(in.MovementType) ||
		in.MovementType == stockMovementDomain.TypeSale ||
		in.MovementType == stockMovementDomain.TypeRefund {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidMovementType)
	}
	isPurchase := in.IsPurchase != nil && *in.IsPurchase
	// An operator may record that merchandise arrived and leave the purchase
	// pending for the owner. Only the owner can assert that money already left
	// Caja, Fondo or an external source. Keep this in the use case (not only in
	// React/the HTTP handler) so offline and future interfaces cannot bypass it.
	if isPurchase && in.ActorRole != "owner" && strings.TrimSpace(in.PurchaseStatus) != purchaseDomain.StatusUnpaid {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrPurchaseOwnerRequired, "")
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" || len(key) > 120 {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrAdjustmentIdempotencyRequired)
	}
	in.IdempotencyKey = key
	if isPurchase && in.MovementType != stockMovementDomain.TypeRestock {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchase)
	}
	if isPurchase && uc.Purchases == nil {
		return nil, sharedDomain.NewUnexpectedError(errors.New("inventory purchase repository is not configured"))
	}
	if !isPurchase && (in.PurchaseStatus != "" || in.PaidOn != nil || in.PaymentMethod != "" ||
		in.PaidFrom != "" || in.CashDrawerID != nil) {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchase)
	}
	if in.MovementType != stockMovementDomain.TypeRestock {
		in.Cost = nil // cost has no business meaning outside an inventory entry.
	}
	if in.Cost != nil && !validAdjustmentCost(*in.Cost) {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchaseCost)
	}
	if in.Reason != nil {
		reason := strings.TrimSpace(*in.Reason)
		if reason == "" {
			in.Reason = nil
		} else {
			in.Reason = &reason
		}
	}
	if isPurchase {
		in.PurchaseStatus = strings.TrimSpace(in.PurchaseStatus)
		if in.PurchaseStatus == "" {
			in.PurchaseStatus = purchaseDomain.StatusPaid
		}
		in.PaymentMethod = strings.TrimSpace(in.PaymentMethod)
		in.PaidFrom = normalizedPurchaseSource(in.PaidFrom)
		if in.PaidFrom != purchaseDomain.PaidFromCashDrawer && in.CashDrawerID != nil {
			return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchaseSource)
		}
		if in.PaidOn != nil {
			day := time.Date(in.PaidOn.Year(), in.PaidOn.Month(), in.PaidOn.Day(), 0, 0, 0, 0, time.UTC)
			in.PaidOn = &day
		}
	}
	fingerprint, fingerprintErr := adjustmentCommandFingerprint(in, isPurchase)
	if fingerprintErr != nil {
		return nil, sharedDomain.NewUnexpectedError(fingerprintErr)
	}
	now := time.Now().UTC()
	var out AdjustStockOutput
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		if isPurchase && in.PaidOn != nil {
			paidOn, dateErr := resolvePurchasePaidOn(tx, uc.Gyms, in.GymID, *in.PaidOn, now)
			if dateErr != nil {
				return dateErr
			}
			in.PaidOn = &paidOn
		}
		existingMovement, err := uc.StockMovements.GetByIdempotencyKey(tx, in.GymID, key)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if existingMovement != nil {
			if existingMovement.IdempotencyFingerprint != fingerprint {
				return sharedDomain.NewBusinessError(prodErrors.ErrAdjustmentIdempotencyConflict, "")
			}
			if len(existingMovement.IdempotencyResult) == 0 {
				return sharedDomain.NewUnexpectedError(errors.New("keyed stock adjustment has no replay result"))
			}
			if err = json.Unmarshal(existingMovement.IdempotencyResult, &out); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			return nil
		}

		// Compatibility for purchases created immediately before stock-level
		// idempotency shipped. New rows are always found by the journal lookup.
		if isPurchase {
			existing, err := uc.Purchases.GetByIdempotencyKey(tx, in.GymID, key)
			if err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			if existing != nil {
				same := samePurchaseRequest(existing, in)
				if same && existing.Status == purchaseDomain.StatusPaid && existing.PaidFrom != nil &&
					*existing.PaidFrom == purchaseDomain.PaidFromCashDrawer {
					if existing.CashMovementID == nil || uc.PurchaseCash == nil {
						return sharedDomain.NewUnexpectedError(errors.New("inventory purchase cash recorder is not configured"))
					}
					drawerID := effectivePurchaseDrawerID(in.GymID, in.CashDrawerID)
					same, err = uc.PurchaseCash.InventoryPurchaseUsesCashDrawer(tx, in.GymID, *existing.CashMovementID, drawerID)
					if err != nil {
						return sharedDomain.NewUnexpectedError(err)
					}
				}
				if !same {
					return sharedDomain.NewBusinessError(prodErrors.ErrAdjustmentIdempotencyConflict, "")
				}
				p, err := uc.Products.GetByID(tx, existing.ProductID)
				if err != nil {
					return err
				}
				purchaseID := existing.ID
				out = AdjustStockOutput{
					NewStock: p.Stock, Delta: existing.Quantity,
					MovementID: existing.StockMovementID, PurchaseID: &purchaseID,
					CashMovementID: existing.CashMovementID,
				}
				return nil
			}
		}

		p, err := uc.Products.GetByID(tx, in.ProductID)
		if err != nil {
			return err
		}
		if p.GymID != in.GymID {
			return sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
		}
		var delta int
		switch in.MovementType {
		case stockMovementDomain.TypeRestock:
			if err := p.IncrementStock(in.Quantity, now); err != nil {
				return sharedDomain.NewValidationError(err)
			}
			delta = in.Quantity
		case stockMovementDomain.TypeShrinkage:
			if err := p.DecrementStock(in.Quantity, now); err != nil {
				if err == prodErrors.ErrInsufficientStock {
					return sharedDomain.NewBusinessError(err, "")
				}
				return sharedDomain.NewValidationError(err)
			}
			delta = -in.Quantity
		case stockMovementDomain.TypeCountCorrection:
			d, err := p.SetStock(in.Quantity, now)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			delta = d
		}
		if _, err := uc.Products.Update(tx, p); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		var cost *float64
		if in.MovementType == stockMovementDomain.TypeRestock {
			cost = in.Cost
		}
		movementID := uuid.NewSHA1(in.GymID, []byte("stock-adjustment:"+key))
		var purchaseID, cashMovementID *uuid.UUID
		var purchase *purchaseDomain.Purchase
		if isPurchase {
			var reservedCashID *uuid.UUID
			id := uuid.NewSHA1(in.GymID, []byte("inventory-purchase:"+key))
			purchaseID = &id
			if in.PurchaseStatus == purchaseDomain.StatusPaid && in.PaidFrom == purchaseDomain.PaidFromCashDrawer {
				id := uuid.NewSHA1(*purchaseID, []byte("initial-payment"))
				reservedCashID = &id
			}
			purchase, err = purchaseDomain.New(purchaseDomain.Input{
				ID: *purchaseID, GymID: in.GymID, StockMovementID: movementID,
				ProductID: p.ID, CreatedBy: in.ActorUserID, Quantity: in.Quantity,
				UnitCost: in.Cost, Status: in.PurchaseStatus, PaidOn: in.PaidOn,
				PaymentMethod: in.PaymentMethod, PaidFrom: in.PaidFrom,
				CashMovementID: reservedCashID, IdempotencyKey: key, Now: now,
			})
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			cashMovementID = purchase.CashMovementID
		}

		out = AdjustStockOutput{
			NewStock: p.Stock, Delta: delta, MovementID: movementID,
			PurchaseID: purchaseID, CashMovementID: cashMovementID,
		}
		result, err := json.Marshal(out)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		mv, err := stockMovementDomain.New(movementID, in.GymID, p.ID, in.ActorUserID,
			in.MovementType, delta, in.Reason, cost, nil, now)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if in.MovementType == stockMovementDomain.TypeRestock && !isPurchase {
			mv.MarkAsCapture()
		}
		if err = mv.WithIdempotency(key, fingerprint, result); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err = uc.StockMovements.Create(tx, mv); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		if purchase != nil {
			reservedCashID := purchase.CashMovementID
			if reservedCashID != nil {
				if uc.PurchaseCash == nil {
					return sharedDomain.NewUnexpectedError(errors.New("inventory purchase cash recorder is not configured"))
				}
				reason := fmt.Sprintf("Compra de inventario: %s", p.Name)
				if in.Reason != nil && strings.TrimSpace(*in.Reason) != "" {
					reason = strings.TrimSpace(*in.Reason)
				}
				if _, err := uc.PurchaseCash.RecordInventoryPurchase(ctx, tx,
					*reservedCashID, in.GymID, in.ActorUserID, effectivePurchaseDrawerID(in.GymID, in.CashDrawerID), *purchase.PaidOn,
					*purchase.TotalAmount, reason, now); err != nil {
					return err
				}
			}
			if _, err := uc.Purchases.Create(tx, purchase); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "stock_movements",
			EntityID:    mv.ID,
			Action:      audit.ActionCreate,
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"product_id":       p.ID,
				"movement_type":    mv.MovementType,
				"delta":            mv.Delta,
				"new_stock":        p.Stock,
				"reason":           mv.Reason,
				"cost":             mv.Cost,
				"is_purchase":      isPurchase,
				"purchase_id":      purchaseID,
				"cash_movement_id": cashMovementID,
				"cash_drawer_id":   in.CashDrawerID,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func samePurchaseRequest(p *purchaseDomain.Purchase, in AdjustStockInput) bool {
	status := in.PurchaseStatus
	if status == "" {
		status = purchaseDomain.StatusPaid
	}
	if p.GymID != in.GymID || p.ProductID != in.ProductID || p.Quantity != in.Quantity || p.Status != status {
		return false
	}
	if !sameOptionalMoney(p.UnitCost, in.Cost) {
		return false
	}
	if status == purchaseDomain.StatusUnpaid {
		return true
	}
	return p.PaidOn != nil && in.PaidOn != nil && p.PaidOn.Format("2006-01-02") == in.PaidOn.Format("2006-01-02") &&
		p.PaymentMethod != nil && *p.PaymentMethod == in.PaymentMethod &&
		p.PaidFrom != nil && *p.PaidFrom == normalizedPurchaseSource(in.PaidFrom)
}

func sameOptionalMoney(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Round(*a*100) == math.Round(*b*100)
}

func normalizedPurchaseSource(v string) string {
	if strings.TrimSpace(v) == "cash_register" {
		return purchaseDomain.PaidFromCashDrawer
	}
	return strings.TrimSpace(v)
}

func effectivePurchaseDrawerID(gymID uuid.UUID, requested *uuid.UUID) uuid.UUID {
	if requested != nil && *requested != uuid.Nil {
		return *requested
	}
	return gymID
}

func validAdjustmentCost(v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 9999999999.99 {
		return false
	}
	scaled := v * 100
	return math.Abs(scaled-math.Round(scaled)) <= math.Max(1e-7, math.Abs(scaled)*1e-15)
}

// adjustmentCommandFingerprint describes the semantic command rather than
// Go's pointer representation. Defaults and the one-release cash_register
// alias are normalized before reaching this function, so equivalent retries
// compare equal across desktop/cloud implementations.
func adjustmentCommandFingerprint(in AdjustStockInput, isPurchase bool) (string, error) {
	type command struct {
		GymID          string  `json:"gym_id"`
		ProductID      string  `json:"product_id"`
		MovementType   string  `json:"movement_type"`
		Quantity       int     `json:"quantity"`
		CostCents      *int64  `json:"cost_cents,omitempty"`
		IsPurchase     bool    `json:"is_purchase"`
		PurchaseStatus string  `json:"purchase_status,omitempty"`
		PaidOn         string  `json:"paid_on,omitempty"`
		PaymentMethod  string  `json:"payment_method,omitempty"`
		PaidFrom       string  `json:"paid_from,omitempty"`
		CashDrawerID   string  `json:"cash_drawer_id,omitempty"`
		Reason         *string `json:"reason,omitempty"`
	}
	c := command{
		GymID: in.GymID.String(), ProductID: in.ProductID.String(),
		MovementType: in.MovementType, Quantity: in.Quantity,
		IsPurchase: isPurchase, Reason: in.Reason,
	}
	if in.Cost != nil {
		cents := int64(math.Round(*in.Cost * 100))
		c.CostCents = &cents
	}
	if isPurchase {
		c.PurchaseStatus = in.PurchaseStatus
		if in.PaidOn != nil {
			c.PaidOn = in.PaidOn.Format("2006-01-02")
		}
		c.PaymentMethod = in.PaymentMethod
		c.PaidFrom = in.PaidFrom
		if in.PaidFrom == purchaseDomain.PaidFromCashDrawer {
			c.CashDrawerID = effectivePurchaseDrawerID(in.GymID, in.CashDrawerID).String()
		}
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
