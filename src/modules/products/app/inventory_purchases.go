package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchaseDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type InventoryPurchaseView struct {
	RecordedOn   string
	Receipt      *purchaseDomain.Receipt
	Purchase     *purchaseDomain.Purchase
	ProductName  string
	CashDrawerID *uuid.UUID
}

type ListInventoryPurchasesInput struct {
	ReceiptStatus string
	GymID         uuid.UUID
	Status        string
	From          *time.Time
	To            *time.Time
	Page          int
	PageSize      int
}

type ListInventoryPurchasesOutput struct {
	Items    []InventoryPurchaseView
	Total    int
	Page     int
	PageSize int
}

type ListInventoryPurchases struct {
	Gyms      gymRepo.GymRepository
	Receipts  prodRepo.InventoryPurchaseReceiptRepository
	Purchases prodRepo.InventoryPurchaseRepository
	Products  prodRepo.ProductRepository
	Cash      InventoryPurchaseCashRecorder
	UoW       sharedDomain.UnitOfWork
}

func NewListInventoryPurchases(purchases prodRepo.InventoryPurchaseRepository, products prodRepo.ProductRepository,
	cash InventoryPurchaseCashRecorder, uow sharedDomain.UnitOfWork) *ListInventoryPurchases {
	return &ListInventoryPurchases{Purchases: purchases, Products: products, Cash: cash, UoW: uow}
}

func (uc *ListInventoryPurchases) WithReceipts(r prodRepo.InventoryPurchaseReceiptRepository) *ListInventoryPurchases {
	uc.Receipts = r
	return uc
}

func (uc *ListInventoryPurchases) WithGyms(r gymRepo.GymRepository) *ListInventoryPurchases {
	uc.Gyms = r
	return uc
}

func (uc *ListInventoryPurchases) Execute(ctx context.Context, gymID uuid.UUID, status string) ([]InventoryPurchaseView, error) {
	result, err := uc.ExecuteList(ctx, ListInventoryPurchasesInput{
		GymID: gymID, Status: status, Page: 1, PageSize: prodRepo.MaxPageSize,
	})
	if err != nil {
		return nil, err
	}
	return result.Items, nil
}

func (uc *ListInventoryPurchases) ExecuteList(ctx context.Context, in ListInventoryPurchasesInput) (*ListInventoryPurchasesOutput, error) {
	if in.ReceiptStatus != "" && in.ReceiptStatus != "pending" && in.ReceiptStatus != "received" {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchase)
	}
	status := strings.ToLower(strings.TrimSpace(in.Status))
	if status == "" {
		status = purchaseDomain.StatusUnpaid
	}
	if status != "all" && status != purchaseDomain.StatusUnpaid && status != purchaseDomain.StatusPaid && status != purchaseDomain.StatusLegacyIncomplete {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchase)
	}
	if in.From != nil && in.To != nil && purchaseDateOnly(*in.From).After(purchaseDateOnly(*in.To)) {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchase)
	}
	page := in.Page
	if page < 1 {
		page = 1
	}
	pageSize := in.PageSize
	if pageSize < 1 {
		pageSize = prodRepo.DefaultPageSize
	}
	if pageSize > prodRepo.MaxPageSize {
		pageSize = prodRepo.MaxPageSize
	}
	out := &ListInventoryPurchasesOutput{Items: []InventoryPurchaseView{}, Page: page, PageSize: pageSize}
	err := sharedDomain.ReadSnapshot(ctx, uc.UoW, func(tx sharedDomain.Transaction) error {
		location, err := purchaseLocation(tx, uc.Gyms, in.GymID)
		if err != nil {
			return err
		}
		// Request dates are gym calendar dates. Storage bounds are UTC instants,
		// with the upper bound at the next local midnight (including DST days).
		boundary := func(day *time.Time, nextDay bool) *time.Time {
			if day == nil {
				return nil
			}
			value := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, location)
			if nextDay {
				value = value.AddDate(0, 0, 1)
			}
			value = value.UTC()
			return &value
		}
		rows, total, err := uc.Purchases.List(tx, prodRepo.InventoryPurchaseListQuery{
			ReceiptStatus: in.ReceiptStatus, GymID: in.GymID, Status: status, From: boundary(in.From, false), To: boundary(in.To, true),
			Page: page, PageSize: pageSize,
		})
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		out.Total = total
		out.Items = make([]InventoryPurchaseView, 0, len(rows))
		for _, p := range rows {
			view, viewErr := inventoryPurchaseView(tx, uc.Products, uc.Cash, p, uc.Receipts)
			if viewErr != nil {
				return viewErr
			}

			view.RecordedOn = p.CreatedAt.In(location).Format("2006-01-02")
			out.Items = append(out.Items, view)
		}
		return nil
	})
	return out, err
}

type PayInventoryPurchaseInput struct {
	GymID, ActorUserID, PurchaseID uuid.UUID
	ActorRole                      string
	ExpectedVersion                int
	PaidOn                         time.Time
	PaymentMethod, PaidFrom        string
	CashDrawerID                   *uuid.UUID
	CashMovementID                 *uuid.UUID
	IdempotencyKey                 string
}

type PayInventoryPurchase struct {
	Receipts  prodRepo.InventoryPurchaseReceiptRepository
	Purchases prodRepo.InventoryPurchaseRepository
	Products  prodRepo.ProductRepository
	Cash      InventoryPurchaseCashRecorder
	Gyms      gymRepo.GymRepository
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
}

func NewPayInventoryPurchase(purchases prodRepo.InventoryPurchaseRepository, products prodRepo.ProductRepository,
	cash InventoryPurchaseCashRecorder, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *PayInventoryPurchase {
	return &PayInventoryPurchase{Purchases: purchases, Products: products, Cash: cash, UoW: uow, Audit: recorder}
}

func (uc *PayInventoryPurchase) WithGyms(gyms gymRepo.GymRepository) *PayInventoryPurchase {
	uc.Gyms = gyms
	return uc
}

func (uc *PayInventoryPurchase) Execute(ctx context.Context, in PayInventoryPurchaseInput) (*InventoryPurchaseView, error) {
	if in.ActorRole != "owner" {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrPurchaseOwnerRequired, "")
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" || len(key) > 120 {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrPurchaseIdempotencyRequired)
	}
	if in.ExpectedVersion < 1 {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrPurchaseVersionConflict)
	}
	paidFrom := normalizedPurchaseSource(in.PaidFrom)
	if paidFrom != purchaseDomain.PaidFromCashDrawer && in.CashDrawerID != nil {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchaseSource)
	}
	if paidFrom != purchaseDomain.PaidFromCashDrawer && in.CashMovementID != nil {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrInvalidPurchaseSource)
	}
	var out InventoryPurchaseView
	now := time.Now().UTC()
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		paidOn, dateErr := resolvePurchasePaidOn(tx, uc.Gyms, in.GymID, in.PaidOn, now)
		if dateErr != nil {
			return dateErr
		}
		in.PaidOn = paidOn
		p, err := uc.Purchases.GetByID(tx, in.GymID, in.PurchaseID)
		if err != nil {
			return err
		}
		if p.Status == purchaseDomain.StatusPaid {
			same, compareErr := sameInventoryPurchasePayment(tx, uc.Cash, p, in.PaidOn, in.PaymentMethod, paidFrom,
				effectivePurchaseDrawerID(in.GymID, in.CashDrawerID), in.CashMovementID)
			if compareErr != nil {
				return sharedDomain.NewUnexpectedError(compareErr)
			}
			if !same {
				return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseAlreadyResolved, "")
			}
			out, err = inventoryPurchaseView(tx, uc.Products, uc.Cash, p, uc.Receipts)
			return err
		}
		if p.Version != in.ExpectedVersion {
			return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseVersionConflict, "")
		}
		product, err := uc.Products.GetByID(tx, p.ProductID)
		if err != nil {
			return err
		}
		if product.GymID != in.GymID {
			return sharedDomain.NewBusinessError(prodErrors.ErrCrossGym, "")
		}
		var movementID *uuid.UUID
		if paidFrom == purchaseDomain.PaidFromCashDrawer {
			if in.CashMovementID != nil {
				id := *in.CashMovementID
				movementID = &id
			} else {
				id := uuid.NewSHA1(p.ID, []byte("inventory-purchase-payment:"+strconv.Itoa(p.Version)))
				movementID = &id
			}
		}
		version := p.Version
		if err = p.MarkPaid(in.PaidOn, in.PaymentMethod, paidFrom, movementID, now); err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if movementID != nil {
			if uc.Cash == nil {
				return sharedDomain.NewUnexpectedError(fmt.Errorf("inventory purchase cash recorder is not configured"))
			}
			if in.CashMovementID != nil {
				if _, err = uc.Cash.UseExistingInventoryPurchaseCash(ctx, tx, *movementID, in.GymID, in.ActorUserID,
					in.CashDrawerID, *p.PaidOn, *p.TotalAmount, now); err != nil {
					return err
				}
			} else {
				reason := "Pago compra de inventario: " + product.Name
				if _, err = uc.Cash.RecordInventoryPurchase(ctx, tx, *movementID, in.GymID, in.ActorUserID,
					effectivePurchaseDrawerID(in.GymID, in.CashDrawerID), *p.PaidOn, *p.TotalAmount, reason, now); err != nil {
					return err
				}
			}
		}
		if _, err = uc.Purchases.Update(tx, p, version); err != nil {
			if errors.Is(err, prodErrors.ErrRemotePurchaseCloudOnly) {
				return sharedDomain.NewBusinessError(prodErrors.ErrRemotePurchaseCloudOnly, "")
			}
			if err == prodErrors.ErrPurchaseVersionConflict {
				return sharedDomain.NewBusinessError(err, "")
			}
			return sharedDomain.NewUnexpectedError(err)
		}
		if err = recordInventoryPurchaseAudit(ctx, uc.Audit, tx, p, in.ActorUserID, "mark_paid", map[string]any{
			"paid_on": p.PaidOn, "payment_method": p.PaymentMethod, "paid_from": p.PaidFrom,
			"cash_movement_id": p.CashMovementID, "cash_drawer_id": in.CashDrawerID,
			"linked_existing_cash_movement": in.CashMovementID != nil, "idempotency_key": key,
		}, now); err != nil {
			return err
		}
		out, err = inventoryPurchaseView(tx, uc.Products, uc.Cash, p, uc.Receipts)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type ReopenInventoryPurchaseInput struct {
	GymID, ActorUserID, PurchaseID uuid.UUID
	ActorRole                      string
	ExpectedVersion                int
	CorrectionReason               string
}

type ReopenInventoryPurchase struct {
	Receipts  prodRepo.InventoryPurchaseReceiptRepository
	Purchases prodRepo.InventoryPurchaseRepository
	Products  prodRepo.ProductRepository
	Cash      InventoryPurchaseCashRecorder
	UoW       sharedDomain.UnitOfWork
	Audit     audit.Recorder
}

func NewReopenInventoryPurchase(purchases prodRepo.InventoryPurchaseRepository, products prodRepo.ProductRepository,
	cash InventoryPurchaseCashRecorder, uow sharedDomain.UnitOfWork, recorder audit.Recorder) *ReopenInventoryPurchase {
	return &ReopenInventoryPurchase{Purchases: purchases, Products: products, Cash: cash, UoW: uow, Audit: recorder}
}

func (uc *ReopenInventoryPurchase) Execute(ctx context.Context, in ReopenInventoryPurchaseInput) (*InventoryPurchaseView, error) {
	if in.ActorRole != "owner" {
		return nil, sharedDomain.NewBusinessError(prodErrors.ErrPurchaseOwnerRequired, "")
	}
	reason := strings.TrimSpace(in.CorrectionReason)
	if utf8.RuneCountInString(reason) < 3 || utf8.RuneCountInString(reason) > 200 {
		return nil, sharedDomain.NewValidationError(prodErrors.ErrPurchaseCorrectionReason)
	}
	var out InventoryPurchaseView
	now := time.Now().UTC()
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		p, err := uc.Purchases.GetByID(tx, in.GymID, in.PurchaseID)
		if err != nil {
			return err
		}
		if p.Status == purchaseDomain.StatusUnpaid {
			out, err = inventoryPurchaseView(tx, uc.Products, uc.Cash, p, uc.Receipts)
			return err
		}
		if p.Version != in.ExpectedVersion {
			return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseVersionConflict, "")
		}
		if p.Status != purchaseDomain.StatusPaid {
			return sharedDomain.NewBusinessError(prodErrors.ErrPurchaseAlreadyResolved, "")
		}
		previous := map[string]any{"paid_on": p.PaidOn, "payment_method": p.PaymentMethod,
			"paid_from": p.PaidFrom, "cash_movement_id": p.CashMovementID}
		if p.CashMovementID != nil {
			if uc.Cash == nil {
				return sharedDomain.NewUnexpectedError(fmt.Errorf("inventory purchase cash recorder is not configured"))
			}
			if err = uc.Cash.VoidInventoryPurchaseCash(ctx, tx, in.GymID, in.ActorUserID, *p.CashMovementID, reason, now); err != nil {
				return err
			}
		}
		version := p.Version
		if err = p.ReopenPayment(now); err != nil {
			return sharedDomain.NewBusinessError(err, "")
		}
		if _, err = uc.Purchases.Update(tx, p, version); err != nil {
			if errors.Is(err, prodErrors.ErrRemotePurchaseCloudOnly) {
				return sharedDomain.NewBusinessError(prodErrors.ErrRemotePurchaseCloudOnly, "")
			}
			return sharedDomain.NewUnexpectedError(err)
		}
		if err = recordInventoryPurchaseAudit(ctx, uc.Audit, tx, p, in.ActorUserID, "reopen_payment", map[string]any{
			"before": previous, "status_after": p.Status, "correction_reason": reason,
		}, now); err != nil {
			return err
		}
		out, err = inventoryPurchaseView(tx, uc.Products, uc.Cash, p, uc.Receipts)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func sameInventoryPurchasePayment(tx sharedDomain.Transaction, cash InventoryPurchaseCashRecorder,
	p *purchaseDomain.Purchase, paidOn time.Time, method, paidFrom string, drawerID uuid.UUID,
	requestedMovementID *uuid.UUID) (bool, error) {
	if p.PaidOn == nil || p.PaymentMethod == nil || p.PaidFrom == nil ||
		p.PaidOn.Format("2006-01-02") != paidOn.Format("2006-01-02") || *p.PaymentMethod != method || *p.PaidFrom != paidFrom {
		return false, nil
	}
	if paidFrom != purchaseDomain.PaidFromCashDrawer {
		return p.CashMovementID == nil && requestedMovementID == nil, nil
	}
	if p.CashMovementID == nil || cash == nil {
		return false, nil
	}
	if requestedMovementID != nil {
		return *p.CashMovementID == *requestedMovementID, nil
	}
	// A retry which omits cash_movement_id is equivalent only when the
	// original command created Tinta's deterministic outflow. It must not
	// silently replay a command that originally linked a manual CashOut.
	createdID := uuid.NewSHA1(p.ID, []byte("inventory-purchase-payment:"+strconv.Itoa(p.Version-1)))
	if *p.CashMovementID != createdID {
		return false, nil
	}
	return cash.InventoryPurchaseUsesCashDrawer(tx, p.GymID, *p.CashMovementID, drawerID)
}

func inventoryPurchaseView(tx sharedDomain.Transaction, products prodRepo.ProductRepository, cash InventoryPurchaseCashRecorder,
	p *purchaseDomain.Purchase, receipts ...prodRepo.InventoryPurchaseReceiptRepository) (InventoryPurchaseView, error) {
	product, err := products.GetByID(tx, p.ProductID)
	if err != nil {
		return InventoryPurchaseView{}, err
	}
	view := InventoryPurchaseView{Purchase: p, ProductName: product.Name}
	if p.HasSeparateReceipt() && len(receipts) > 0 && receipts[0] != nil {
		view.Receipt, err = receipts[0].GetByPurchase(tx, p.GymID, p.ID)
		if err != nil {
			return view, err
		}
	}
	if p.CashMovementID != nil && cash != nil {
		view.CashDrawerID, err = cash.CashDrawerForInventoryPurchase(tx, p.GymID, *p.CashMovementID)
	}
	return view, err
}

func recordInventoryPurchaseAudit(ctx context.Context, recorder audit.Recorder, tx sharedDomain.Transaction,
	p *purchaseDomain.Purchase, actor uuid.UUID, action string, changes map[string]any, now time.Time) error {
	if err := recorder.Record(ctx, tx, audit.Entry{GymID: p.GymID, EntityType: "inventory_purchases", EntityID: p.ID,
		Action: action, ActorUserID: &actor, Changes: changes, IPAddress: audit.IPFromContext(ctx),
		UserAgent: audit.UAFromContext(ctx), At: now}); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	return nil
}

func samePurchaseMoney(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Round(*a*100) == math.Round(*b*100)
}

func purchaseDateOnly(v time.Time) time.Time {
	return time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC)
}

func (uc *PayInventoryPurchase) WithReceipts(r prodRepo.InventoryPurchaseReceiptRepository) *PayInventoryPurchase {
	uc.Receipts = r
	return uc
}

func (uc *ReopenInventoryPurchase) WithReceipts(r prodRepo.InventoryPurchaseReceiptRepository) *ReopenInventoryPurchase {
	uc.Receipts = r
	return uc
}
