package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchaseDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	stockDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CorrectInventoryPurchaseInput struct {
	GymID, ActorUserID, PurchaseID uuid.UUID
	ActorRole                      string
	ExpectedVersion                int
	Quantity                       int
	UnitCost                       *float64
	Annul                          bool
	CorrectionReason               string
	IdempotencyKey                 string
}

// CorrectInventoryPurchaseOutput is also persisted as the exact replay
// result on the keyed reversal stock movement.
type CorrectInventoryPurchaseOutput struct {
	ID                    uuid.UUID   `json:"id"`
	Version               int         `json:"version"`
	StockMovementID       uuid.UUID   `json:"stock_movement_id"`
	ProductID             uuid.UUID   `json:"product_id"`
	ProductName           string      `json:"product_name"`
	Quantity              int         `json:"quantity"`
	UnitCost              *float64    `json:"unit_cost"`
	TotalAmount           *float64    `json:"total_amount"`
	Status                string      `json:"status"`
	PaidOn                *string     `json:"paid_on"`
	PaymentMethod         *string     `json:"payment_method"`
	PaidFrom              *string     `json:"paid_from"`
	CashMovementID        *uuid.UUID  `json:"cash_movement_id"`
	CashDrawerID          *uuid.UUID  `json:"cash_drawer_id"`
	RecordedOn            string      `json:"recorded_on"`
	CreatedBy             uuid.UUID   `json:"created_by"`
	CreatedAt             time.Time   `json:"created_at"`
	UpdatedAt             time.Time   `json:"updated_at"`
	Annulled              bool        `json:"annulled"`
	StockDelta            int         `json:"stock_delta"`
	NewStock              int         `json:"new_stock"`
	CorrectionMovementIDs []uuid.UUID `json:"correction_movement_ids"`
}

type CorrectInventoryPurchase struct {
	Cash      InventoryPurchaseCashRecorder
	Purchases prodRepo.InventoryPurchaseRepository
	Products  prodRepo.ProductRepository
	Movements prodRepo.StockMovementRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
}

func NewCorrectInventoryPurchase(purchases prodRepo.InventoryPurchaseRepository, products prodRepo.ProductRepository,
	movements prodRepo.StockMovementRepository, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *CorrectInventoryPurchase {
	return &CorrectInventoryPurchase{Purchases: purchases, Products: products, Movements: movements, UoW: uow, Audit: recorder}
}

func (uc *CorrectInventoryPurchase) WithCash(cash InventoryPurchaseCashRecorder) *CorrectInventoryPurchase {
	uc.Cash = cash
	return uc
}

func (uc *CorrectInventoryPurchase) Execute(ctx context.Context, in CorrectInventoryPurchaseInput) (*CorrectInventoryPurchaseOutput, error) {
	if in.ActorRole != "owner" {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrPurchaseOwnerRequired, "")
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" || len(key) > 120 {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrPurchaseIdempotencyRequired)
	}
	reason := strings.TrimSpace(in.CorrectionReason)
	if utf8.RuneCountInString(reason) < 3 || utf8.RuneCountInString(reason) > 200 {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrPurchaseCorrectionReason)
	}
	if in.ExpectedVersion < 1 || (!in.Annul && (in.Quantity <= 0 || in.UnitCost == nil)) ||
		(in.Annul && (in.Quantity != 0 || in.UnitCost != nil)) {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchase)
	}
	if in.UnitCost != nil && !validAdjustmentCost(*in.UnitCost) {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchaseCost)
	}
	fingerprint, err := inventoryPurchaseCorrectionFingerprint(in, reason)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	now := time.Now().UTC()
	var out CorrectInventoryPurchaseOutput
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		existing, lookupErr := uc.Movements.GetByIdempotencyKey(tx, in.GymID, key)
		if lookupErr != nil {
			return sharedDomain.NewUnexpectedError(lookupErr)
		}
		if existing != nil {
			if existing.IdempotencyFingerprint != fingerprint {
				return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseCorrectionConflict, "")
			}
			if len(existing.IdempotencyResult) == 0 {
				return sharedDomain.NewUnexpectedError(errors.New("purchase correction has no replay result"))
			}
			if replayErr := json.Unmarshal(existing.IdempotencyResult, &out); replayErr != nil {
				return sharedDomain.NewUnexpectedError(replayErr)
			}
			return nil
		}

		p, getErr := uc.Purchases.GetByID(tx, in.GymID, in.PurchaseID)
		if getErr != nil {
			return getErr
		}
		if p.Version != in.ExpectedVersion {
			return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseVersionConflict, "")
		}
		originalVersion, originalStatus := p.Version, p.Status
		paidOn, method, paidFrom := p.PaidOn, p.PaymentMethod, p.PaidFrom
		var drawerID *uuid.UUID
		if originalStatus == purchaseDomain.StatusPaid {
			if paidOn == nil || method == nil || paidFrom == nil {
				return sharedDomain.NewBusinessError(prodErrors.ErrInvalidPurchase, "")
			}
			if p.CashMovementID != nil {
				if uc.Cash == nil {
					return sharedDomain.NewUnexpectedError(errors.New("purchase cash recorder missing"))
				}
				drawerID, getErr = uc.Cash.CashDrawerForInventoryPurchase(tx, in.GymID, *p.CashMovementID)
				if getErr != nil {
					return getErr
				}
				if getErr = uc.Cash.VoidInventoryPurchaseCash(ctx, tx, in.GymID, in.ActorUserID, *p.CashMovementID, reason, now); getErr != nil {
					return getErr
				}
			}
			if getErr = p.ReopenPayment(now); getErr != nil {
				return getErr
			}
		}
		if p.Status != purchaseDomain.StatusUnpaid || p.UnitCost == nil || p.TotalAmount == nil {
			return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseAlreadyResolved, "")
		}
		product, getErr := uc.Products.GetByID(tx, p.ProductID)
		if getErr != nil {
			return getErr
		}
		if product.GymID != in.GymID {
			return sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
		}

		oldQuantity, oldCost, oldTotal, purchaseVersion := p.Quantity, *p.UnitCost, *p.TotalAmount, originalVersion
		newQuantity := in.Quantity
		if in.Annul {
			newQuantity = 0
		}
		stockDelta := newQuantity - oldQuantity
		if p.HasSeparateReceipt() {
			stockDelta = 0
		}
		if stockDelta > 0 {
			if getErr = product.IncrementStock(stockDelta, now); getErr != nil {
				return sharedDomain.NewValidationError(getErr)
			}
		} else if stockDelta < 0 {
			if getErr = product.DecrementStock(-stockDelta, now); getErr != nil {
				if errors.Is(getErr, prodErrors.ErrInsufficientStock) {
					return sharedDomain.NewBusinessError(getErr, "no puedes retirar más unidades de las que quedan en inventario")
				}
				return sharedDomain.NewValidationError(getErr)
			}
		}
		if in.Annul {
			getErr = p.AnnulUnpaid(now)
		} else {
			getErr = p.CorrectUnpaid(in.Quantity, *in.UnitCost, now)
		}
		if getErr != nil {
			if errors.Is(getErr, prodErrors.ErrPurchaseCorrectionNoChange) || errors.Is(getErr, prodErrors.ErrPurchaseAlreadyResolved) {
				return sharedDomain.NewBusinessError(getErr, "")
			}
			return sharedDomain.NewValidationError(getErr)
		}

		if originalStatus == purchaseDomain.StatusPaid && !in.Annul {
			var cashID *uuid.UUID
			if *paidFrom == purchaseDomain.PaidFromCashDrawer {
				if uc.Cash == nil {
					return sharedDomain.NewUnexpectedError(errors.New("purchase cash recorder missing"))
				}
				drawer := in.GymID
				if drawerID != nil {
					drawer = *drawerID
				}
				id := uuid.NewSHA1(p.ID, []byte("purchase-correction:"+key+":cash"))
				recorded, cashErr := uc.Cash.RecordInventoryPurchase(ctx, tx, id, in.GymID, in.ActorUserID, drawer, *paidOn, *p.TotalAmount, "Compra de "+product.Name, now)
				if cashErr != nil {
					return cashErr
				}
				cashID = &recorded
			}
			if getErr = p.MarkPaid(*paidOn, *method, *paidFrom, cashID, now); getErr != nil {
				return getErr
			}
		}

		reversalID := uuid.NewSHA1(p.ID, []byte("purchase-correction:"+key+":reverse"))
		movementIDs := []uuid.UUID{reversalID}
		var replacementID *uuid.UUID
		if !in.Annul && !p.HasSeparateReceipt() {
			id := uuid.NewSHA1(p.ID, []byte("purchase-correction:"+key+":replacement"))
			replacementID = &id
			movementIDs = append(movementIDs, id)
		}
		out = correctionOutput(p, product.Name, product.Stock, stockDelta, movementIDs)
		if !in.Annul {
			out.CashDrawerID = drawerID
		}
		result, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			return sharedDomain.NewUnexpectedError(marshalErr)
		}
		reversalReason := "Reversa por corrección de compra: " + reason
		reversalCost := oldCost
		reversalDelta := -oldQuantity
		if p.HasSeparateReceipt() {
			reversalDelta = 0
		}
		reversal, movementErr := stockDomain.New(reversalID, in.GymID, p.ProductID, in.ActorUserID,
			stockDomain.TypeRestock, reversalDelta, &reversalReason, &reversalCost, nil, now)
		if movementErr != nil {
			return sharedDomain.NewValidationError(movementErr)
		}
		// These are journal replacements, not new purchases. Cost analytics
		// still use them to cancel/replace the original weighted-cost entry.
		reversal.MarkAsCapture()
		if movementErr = reversal.WithIdempotency(key, fingerprint, result); movementErr != nil {
			return sharedDomain.NewValidationError(movementErr)
		}
		if _, movementErr = uc.Movements.Create(tx, reversal); movementErr != nil {
			return sharedDomain.NewUnexpectedError(movementErr)
		}
		if replacementID != nil && !p.HasSeparateReceipt() {
			replacementReason := "Reemplazo por corrección de compra: " + reason
			replacementCost := *p.UnitCost
			replacement, newErr := stockDomain.New(*replacementID, in.GymID, p.ProductID, in.ActorUserID,
				stockDomain.TypeRestock, p.Quantity, &replacementReason, &replacementCost, nil, now)
			if newErr != nil {
				return sharedDomain.NewValidationError(newErr)
			}
			replacement.MarkAsCapture()
			if _, newErr = uc.Movements.Create(tx, replacement); newErr != nil {
				return sharedDomain.NewUnexpectedError(newErr)
			}
		}
		if !p.HasSeparateReceipt() {
			if _, getErr = uc.Products.Update(tx, product); getErr != nil {
				return sharedDomain.NewUnexpectedError(getErr)
			}
		}
		if _, getErr = uc.Purchases.Update(tx, p, purchaseVersion); getErr != nil {
			if errors.Is(getErr, prodErrors.ErrPurchaseVersionConflict) || errors.Is(getErr, prodErrors.ErrRemotePurchaseCloudOnly) {
				return sharedDomain.NewBusinessError(getErr, "")
			}
			return sharedDomain.NewUnexpectedError(getErr)
		}
		return recordInventoryPurchaseAudit(ctx, uc.Audit, tx, p, in.ActorUserID,
			map[bool]string{true: "annul", false: "correct"}[in.Annul], map[string]any{
				"before":      map[string]any{"quantity": oldQuantity, "unit_cost": oldCost, "total_amount": oldTotal, "status": originalStatus},
				"after":       map[string]any{"quantity": p.Quantity, "unit_cost": p.UnitCost, "total_amount": p.TotalAmount, "status": p.Status},
				"stock_delta": stockDelta, "new_stock": product.Stock, "correction_movement_ids": movementIDs,
				"correction_reason": reason, "idempotency_key": key,
			}, now)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func correctionOutput(p *purchaseDomain.Purchase, productName string, newStock, stockDelta int, movementIDs []uuid.UUID) CorrectInventoryPurchaseOutput {
	var paidOn *string
	if p.PaidOn != nil {
		v := p.PaidOn.Format("2006-01-02")
		paidOn = &v
	}
	return CorrectInventoryPurchaseOutput{
		ID: p.ID, Version: p.Version, StockMovementID: p.StockMovementID,
		ProductID: p.ProductID, ProductName: productName, Quantity: p.Quantity,
		UnitCost: copyFloat(p.UnitCost), TotalAmount: copyFloat(p.TotalAmount), Status: p.Status,
		PaidOn: paidOn, PaymentMethod: copyString(p.PaymentMethod), PaidFrom: copyString(p.PaidFrom),
		CashMovementID: copyUUID(p.CashMovementID), RecordedOn: p.CreatedAt.Format("2006-01-02"),
		CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		Annulled: p.Status == purchaseDomain.StatusAnnulled, StockDelta: stockDelta, NewStock: newStock,
		CorrectionMovementIDs: append([]uuid.UUID(nil), movementIDs...),
	}
}

func inventoryPurchaseCorrectionFingerprint(in CorrectInventoryPurchaseInput, reason string) (string, error) {
	type semanticCommand struct {
		GymID, PurchaseID string
		ExpectedVersion   int
		Quantity          int
		UnitCostCents     *int64
		Annul             bool
		CorrectionReason  string
	}
	command := semanticCommand{GymID: in.GymID.String(), PurchaseID: in.PurchaseID.String(),
		ExpectedVersion: in.ExpectedVersion, Quantity: in.Quantity, Annul: in.Annul, CorrectionReason: reason}
	if in.UnitCost != nil {
		cents := int64(math.Round(*in.UnitCost * 100))
		command.UnitCostCents = &cents
	}
	raw, err := json.Marshal(command)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func copyFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}

func copyString(v *string) *string {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}

func copyUUID(v *uuid.UUID) *uuid.UUID {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
