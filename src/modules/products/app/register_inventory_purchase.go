package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
	purchase "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	repo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

type PurchaseLineInput struct {
	ProductID uuid.UUID `json:"product_id"`
	Quantity  int       `json:"quantity"`
	UnitCost  float64   `json:"unit_cost"`
	// Omit nil to preserve fingerprints of submissions from earlier clients.
	NewProduct *PurchaseProductInput `json:"new_product,omitempty"`
}
type PurchaseProductInput struct {
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}
type RegisterInventoryPurchaseInput struct {
	ID, GymID, ActorUserID  uuid.UUID
	ActorRole               string
	Items                   []PurchaseLineInput
	Received, Paid          bool
	PaidOn                  time.Time
	PaymentMethod, PaidFrom string
	CashDrawerID            *uuid.UUID
}

// RegisterInventoryPurchase captures one form atomically. Each product retains
// its own purchase identity for the existing payment/receipt history. The full
// original request fingerprint is immutable on every line, so a lost response
// can be replayed even after a later payment or receipt changed its state.
type RegisterInventoryPurchase struct {
	Purchases repo.InventoryPurchaseRepository
	Receipts  repo.InventoryPurchaseReceiptRepository
	Products  repo.ProductRepository
	Gyms      gymRepo.GymRepository
	Cash      InventoryPurchaseCashRecorder
	UoW       shared.UnitOfWork
	Audit     audit.Recorder
	Origin    string
}

func NewRegisterInventoryPurchase(p repo.InventoryPurchaseRepository, r repo.InventoryPurchaseReceiptRepository, products repo.ProductRepository, gyms gymRepo.GymRepository, cash InventoryPurchaseCashRecorder, uow shared.UnitOfWork, recorder audit.Recorder, origin string) *RegisterInventoryPurchase {
	return &RegisterInventoryPurchase{p, r, products, gyms, cash, uow, recorder, origin}
}
func (uc *RegisterInventoryPurchase) Execute(ctx context.Context, in RegisterInventoryPurchaseInput) ([]InventoryPurchaseView, error) {
	if in.ID == uuid.Nil || len(in.Items) == 0 || len(in.Items) > 100 || (uc.Origin != "desktop" && uc.Origin != "cloud") {
		return nil, shared.NewValidationError(prodErrors.ErrInvalidPurchase)
	}
	// Reception already has permission to record local cash outflows. Paying a
	// purchase uses that same permission, but never grants access to remote funds.
	operatorCashPayment := in.PaidFrom == purchase.PaidFromCashDrawer && in.PaymentMethod == purchase.MethodCash
	if in.ActorRole != "owner" && (in.ActorRole != "operator" || uc.Origin != "desktop" || (in.Paid && !operatorCashPayment)) {
		return nil, shared.NewBusinessError(prodErrors.ErrPurchaseOwnerRequired, "")
	}
	if !in.Paid && (!in.PaidOn.IsZero() || in.PaymentMethod != "" || in.PaidFrom != "" || in.CashDrawerID != nil) {
		return nil, shared.NewValidationError(prodErrors.ErrIncompletePurchasePayment)
	}
	if in.Paid && (in.PaidOn.IsZero() || (uc.Origin == "cloud" && in.PaidFrom == purchase.PaidFromCashDrawer) || (in.PaidFrom != purchase.PaidFromCashDrawer && in.CashDrawerID != nil)) {
		return nil, shared.NewValidationError(prodErrors.ErrInvalidPurchaseSource)
	}
	seen := map[uuid.UUID]bool{}
	for _, line := range in.Items {
		if seen[line.ProductID] || line.ProductID == uuid.Nil {
			return nil, shared.NewValidationError(prodErrors.ErrInvalidPurchase)
		}
		seen[line.ProductID] = true
	}
	// Exclude the operator from semantic identity: another authorized operator
	// may retry a saved submission after a session change.
	fingerprintInput := in
	fingerprintInput.ActorUserID = uuid.Nil
	fingerprintInput.ActorRole = ""
	payload, err := json.Marshal(fingerprintInput)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(payload)
	key := "purchase-form:" + hex.EncodeToString(hash[:])
	now := time.Now().UTC()
	out := make([]InventoryPurchaseView, 0, len(in.Items))
	err = uc.UoW.Command(ctx, func(tx shared.Transaction) error {
		if err := uc.Purchases.LockRegistration(tx, in.GymID, in.ID); err != nil {
			return err
		}
		firstID := uuid.NewSHA1(in.ID, []byte("purchase-line:0"))
		existing, err := uc.Purchases.GetByID(tx, in.GymID, firstID)
		if err != nil && !errors.Is(err, prodErrors.ErrPurchaseNotFound) {
			return err
		}
		replay := existing != nil
		if replay && existing.IdempotencyKey != key+":0" {
			return shared.NewBusinessError(prodErrors.ErrAdjustmentIdempotencyConflict, "")
		}
		var day *time.Time
		if in.Paid {
			value, e := resolvePurchasePaidOn(tx, uc.Gyms, in.GymID, in.PaidOn, now)
			if e != nil {
				return e
			}
			day = &value
		}
		for i, line := range in.Items {
			id := uuid.NewSHA1(in.ID, []byte(fmt.Sprintf("purchase-line:%d", i)))
			var product *productDomain.Product
			var e error
			if line.NewProduct != nil && !replay {
				product, e = uc.createProduct(ctx, tx, in, line, now)
			} else {
				product, e = uc.Products.GetByID(tx, line.ProductID)
			}
			if e != nil {
				return e
			}
			if product.GymID != in.GymID {
				return shared.NewBusinessError(prodErrors.ErrCrossGym, "")
			}
			if replay {
				p, e := uc.Purchases.GetByID(tx, in.GymID, id)
				if e != nil {
					return e
				}
				if p.IdempotencyKey != fmt.Sprintf("%s:%d", key, i) {
					return shared.NewBusinessError(prodErrors.ErrAdjustmentIdempotencyConflict, "")
				}
				receipt, e := uc.Receipts.GetByPurchase(tx, in.GymID, id)
				if e != nil {
					return e
				}
				out = append(out, InventoryPurchaseView{Purchase: p, ProductName: product.Name, Receipt: receipt})
				continue
			}
			if !product.Active {
				return shared.NewBusinessError(prodErrors.ErrProductInactive, "")
			}
			status := purchase.StatusUnpaid
			var cashID *uuid.UUID
			if in.Paid {
				status = purchase.StatusPaid
				if in.PaidFrom == purchase.PaidFromCashDrawer {
					v := uuid.NewSHA1(id, []byte("initial-payment"))
					cashID = &v
				}
			}
			p, e := purchase.New(purchase.Input{ID: id, GymID: in.GymID, ProductID: line.ProductID, CreatedBy: in.ActorUserID, Origin: uc.Origin, AwaitingReceipt: true, Quantity: line.Quantity, UnitCost: &line.UnitCost, Status: status, PaidOn: day, PaymentMethod: in.PaymentMethod, PaidFrom: in.PaidFrom, CashMovementID: cashID, IdempotencyKey: fmt.Sprintf("%s:%d", key, i), Now: now})
			if e != nil {
				return shared.NewValidationError(e)
			}
			if cashID != nil {
				if uc.Cash == nil {
					return shared.NewValidationError(prodErrors.ErrInvalidPurchaseSource)
				}
				if _, e = uc.Cash.RecordInventoryPurchase(ctx, tx, *cashID, in.GymID, in.ActorUserID, effectivePurchaseDrawerID(in.GymID, in.CashDrawerID), *day, *p.TotalAmount, "Compra: "+product.Name, now); e != nil {
					return e
				}
			}
			if _, e = uc.Purchases.Create(tx, p); e != nil {
				return e
			}
			var receipt *purchase.Receipt
			if in.Received {
				receipt, e = purchase.NewReceipt(p, in.ActorUserID, line.Quantity, now)
				if e != nil {
					return shared.NewValidationError(e)
				}
				receipt, e = uc.Receipts.Create(tx, receipt)
				if e != nil {
					return e
				}
			}
			if e = recordInventoryPurchaseAudit(ctx, uc.Audit, tx, p, in.ActorUserID, "register", map[string]any{"registration_id": in.ID, "received": in.Received, "paid": in.Paid}, now); e != nil {
				return e
			}
			out = append(out, InventoryPurchaseView{Purchase: p, ProductName: product.Name, Receipt: receipt})
		}
		return nil
	})
	return out, err
}

// New catalog items have zero stock. Only the receipt adds units; creating the
// product, purchase, receipt, payment and sync events shares one transaction.
func (uc *RegisterInventoryPurchase) createProduct(ctx context.Context, tx shared.Transaction, in RegisterInventoryPurchaseInput, line PurchaseLineInput, now time.Time) (*productDomain.Product, error) {
	if _, err := uc.Products.GetByID(tx, line.ProductID); err == nil {
		return nil, shared.NewValidationError(prodErrors.ErrInvalidPurchase)
	} else if !errors.Is(err, prodErrors.ErrProductNotFound) {
		return nil, err
	}
	details := line.NewProduct
	if math.IsNaN(details.Price) || math.IsInf(details.Price, 0) || details.Price > 99999999.99 || math.Abs(details.Price*100-math.Round(details.Price*100)) > 0.000001 {
		return nil, shared.NewValidationError(prodErrors.ErrInvalidPrice)
	}
	p, err := productDomain.New(line.ProductID, in.GymID, details.Name, details.Price, 0, 0, nil, nil, now)
	if err != nil {
		return nil, shared.NewValidationError(err)
	}
	exists, err := uc.Products.ExistsByGymAndName(tx, in.GymID, p.Name, nil)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, shared.NewBusinessError(prodErrors.ErrNameAlreadyExists, p.Name)
	}
	if _, err = uc.Products.Create(tx, p); err != nil {
		return nil, err
	}
	if err = uc.Audit.Record(ctx, tx, audit.Entry{GymID: in.GymID, EntityType: "products", EntityID: p.ID, Action: audit.ActionCreate, ActorUserID: &in.ActorUserID, Changes: map[string]any{"name": p.Name, "price": p.Price, "stock": 0, "registration_id": in.ID}, IPAddress: audit.IPFromContext(ctx), UserAgent: audit.UAFromContext(ctx), At: now}); err != nil {
		return nil, err
	}
	return p, nil
}
