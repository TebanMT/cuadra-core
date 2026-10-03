// Package purchase models the financial side of an inventory receipt.
// StockMovement remains the physical journal; Purchase answers whether and
// when that receipt was paid, from which location, and for how much.
package purchase

import (
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
)

const (
	StatusUnpaid           = "unpaid"
	StatusPaid             = "paid"
	StatusLegacyIncomplete = "legacy_incomplete"
	StatusAnnulled         = "annulled"

	PaidFromCashDrawer = "cash_drawer"
	PaidFromGymFund    = "gym_fund"
	PaidFromExternal   = "external"

	MethodCash     = "cash"
	MethodTransfer = "transfer"
	MethodCard     = "card"
)

// Purchase records the financial obligation/payment. Legacy local purchases
// link a restock; remote purchases have a separate Receipt after delivery.
// Amounts are pesos in the domain and integer cents at the SQLite edge.
type Purchase struct {
	Origin                                           string
	ID, GymID, StockMovementID, ProductID, CreatedBy uuid.UUID
	Version                                          int
	Quantity                                         int
	UnitCost, TotalAmount                            *float64
	Status                                           string
	PaidOn                                           *time.Time
	PaymentMethod, PaidFrom                          *string
	CashMovementID                                   *uuid.UUID
	IdempotencyKey                                   string
	CreatedAt, UpdatedAt                             time.Time
	DeletedAt                                        *time.Time
}

type Input struct {
	Origin                                           string
	AwaitingReceipt                                  bool
	ID, GymID, StockMovementID, ProductID, CreatedBy uuid.UUID
	Quantity                                         int
	UnitCost                                         *float64
	Status                                           string
	PaidOn                                           *time.Time
	PaymentMethod, PaidFrom                          string
	CashMovementID                                   *uuid.UUID
	IdempotencyKey                                   string
	Now                                              time.Time
}

func New(in Input) (*Purchase, error) {
	if in.ID == uuid.Nil || in.GymID == uuid.Nil || (!in.AwaitingReceipt && in.StockMovementID == uuid.Nil) ||
		in.ProductID == uuid.Nil || in.CreatedBy == uuid.Nil || in.Quantity <= 0 || in.Quantity > 2147483647 {
		return nil, prodErrors.ErrInvalidPurchase
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" || len(key) > 120 {
		return nil, prodErrors.ErrPurchaseIdempotencyRequired
	}
	if in.Status == "" {
		in.Status = StatusPaid
	}
	if in.UnitCost != nil && !validMoney(*in.UnitCost) {
		return nil, prodErrors.ErrInvalidPurchaseCost
	}

	if in.Origin == "" {
		if in.AwaitingReceipt {
			in.Origin = "cloud"
		} else {
			in.Origin = "desktop"
		}
	}
	if in.Origin != "cloud" && in.Origin != "desktop" {
		return nil, prodErrors.ErrInvalidPurchase
	}
	p := &Purchase{
		Origin: in.Origin,
		ID:     in.ID, GymID: in.GymID, StockMovementID: in.StockMovementID,
		ProductID: in.ProductID, CreatedBy: in.CreatedBy, Version: 1,
		Quantity: in.Quantity, UnitCost: copyMoney(in.UnitCost), Status: in.Status,
		CashMovementID: in.CashMovementID, IdempotencyKey: key,
		CreatedAt: in.Now.UTC(), UpdatedAt: in.Now.UTC(),
	}
	if p.UnitCost != nil {
		total := roundCents(*p.UnitCost * float64(p.Quantity))
		if !validMoney(total) {
			return nil, prodErrors.ErrInvalidPurchaseCost
		}
		p.TotalAmount = &total
	}

	switch in.Status {
	case StatusPaid:
		if p.UnitCost == nil || in.PaidOn == nil || in.PaidOn.IsZero() {
			return nil, prodErrors.ErrIncompletePurchasePayment
		}
		if !validMethod(in.PaymentMethod) {
			return nil, prodErrors.ErrInvalidPurchaseMethod
		}
		paidFrom := normalizePaidFrom(in.PaidFrom)
		if !validPaidFrom(paidFrom) {
			return nil, prodErrors.ErrInvalidPurchaseSource
		}
		if paidFrom == PaidFromCashDrawer && (in.PaymentMethod != MethodCash || in.CashMovementID == nil) {
			return nil, prodErrors.ErrIncompletePurchasePayment
		}
		if paidFrom != PaidFromCashDrawer && in.CashMovementID != nil {
			return nil, prodErrors.ErrIncompletePurchasePayment
		}
		day := dateOnly(*in.PaidOn)
		method := in.PaymentMethod
		p.PaidOn, p.PaymentMethod, p.PaidFrom = &day, &method, &paidFrom
	case StatusUnpaid:
		if p.UnitCost == nil || in.PaidOn != nil || in.PaymentMethod != "" || in.PaidFrom != "" || in.CashMovementID != nil {
			return nil, prodErrors.ErrIncompletePurchasePayment
		}
	case StatusLegacyIncomplete:
		// Only migrations/read models should construct this status. It is kept
		// permissive so uncertain historical data stays representable.
	default:
		return nil, prodErrors.ErrInvalidPurchase
	}
	return p, nil
}

// Physical arrival is a separate immutable Receipt for new purchases.
// Its identity never rewrites payment facts; Origin owns financial permissions.
func (p *Purchase) HasSeparateReceipt() bool { return p.StockMovementID == uuid.Nil }

func (p *Purchase) IsCloudManaged() bool {
	return p.Origin == "cloud" || (p.Origin == "" && p.HasSeparateReceipt())
}

func (p *Purchase) IsPaid() bool { return p != nil && p.Status == StatusPaid }

func (p *Purchase) MarkPaid(paidOn time.Time, method, paidFrom string, cashMovementID *uuid.UUID, now time.Time) error {
	if p.Status != StatusUnpaid || p.UnitCost == nil || p.TotalAmount == nil || paidOn.IsZero() {
		return prodErrors.ErrPurchaseAlreadyResolved
	}
	if !validMethod(method) {
		return prodErrors.ErrInvalidPurchaseMethod
	}
	paidFrom = normalizePaidFrom(paidFrom)
	if !validPaidFrom(paidFrom) {
		return prodErrors.ErrInvalidPurchaseSource
	}
	if paidFrom == PaidFromCashDrawer && (method != MethodCash || cashMovementID == nil) {
		return prodErrors.ErrIncompletePurchasePayment
	}
	if paidFrom != PaidFromCashDrawer && cashMovementID != nil {
		return prodErrors.ErrIncompletePurchasePayment
	}
	day := dateOnly(paidOn)
	p.Status = StatusPaid
	p.PaidOn = &day
	p.PaymentMethod = &method
	p.PaidFrom = &paidFrom
	p.CashMovementID = cashMovementID
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}

func (p *Purchase) ReopenPayment(now time.Time) error {
	if p.Status == StatusUnpaid {
		return nil
	}
	if p.Status != StatusPaid {
		return prodErrors.ErrPurchaseAlreadyResolved
	}
	p.Status = StatusUnpaid
	p.PaidOn = nil
	p.PaymentMethod = nil
	p.PaidFrom = nil
	p.CashMovementID = nil
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}

// CorrectUnpaid replaces the receipt's financial quantity/cost. Stock is
// corrected by append-only journal entries in the application transaction;
// this aggregate only owns the canonical purchase amount.
func (p *Purchase) CorrectUnpaid(quantity int, unitCost float64, now time.Time) error {
	if p.Status == StatusPaid {
		return prodErrors.ErrPurchaseMustBeReopened
	}
	if p.Status != StatusUnpaid {
		return prodErrors.ErrPurchaseAlreadyResolved
	}
	if quantity <= 0 || !validMoney(unitCost) {
		return prodErrors.ErrInvalidPurchase
	}
	if p.UnitCost != nil && p.Quantity == quantity &&
		math.Round(*p.UnitCost*100) == math.Round(unitCost*100) {
		return prodErrors.ErrPurchaseCorrectionNoChange
	}
	unitCost = roundCents(unitCost)
	total := roundCents(unitCost * float64(quantity))
	if !validMoney(total) {
		return prodErrors.ErrInvalidPurchaseCost
	}
	p.Quantity = quantity
	p.UnitCost = &unitCost
	p.TotalAmount = &total
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}

func (p *Purchase) AnnulUnpaid(now time.Time) error {
	if p.Status == StatusPaid {
		return prodErrors.ErrPurchaseMustBeReopened
	}
	if p.Status == StatusAnnulled {
		return nil
	}
	if p.Status != StatusUnpaid || p.UnitCost == nil || p.TotalAmount == nil {
		return prodErrors.ErrPurchaseAlreadyResolved
	}
	p.Status = StatusAnnulled
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}

func validMethod(v string) bool {
	return v == MethodCash || v == MethodTransfer || v == MethodCard
}

func validPaidFrom(v string) bool {
	return v == PaidFromCashDrawer || v == PaidFromGymFund || v == PaidFromExternal
}

func normalizePaidFrom(v string) string {
	v = strings.TrimSpace(v)
	if v == "cash_register" { // one-release compatibility with the old API.
		return PaidFromCashDrawer
	}
	return v
}

func validMoney(v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 9999999999.99 {
		return false
	}
	scaled := v * 100
	tolerance := math.Max(1e-7, math.Abs(scaled)*1e-15)
	return math.Abs(scaled-math.Round(scaled)) <= tolerance
}

func roundCents(v float64) float64 { return math.Round(v*100) / 100 }
func dateOnly(v time.Time) time.Time {
	return time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, time.UTC)
}
func copyMoney(v *float64) *float64 {
	if v == nil {
		return nil
	}
	x := roundCents(*v)
	return &x
}

// CompleteLegacyCost adds a documented historical cost without inventing a
// payment date/source or replaying the receipt. Reports retain their existing
// legacy-date fallback until the rest of the historical payment is known.
func (p *Purchase) CompleteLegacyCost(unitCost float64, now time.Time) error {
	if p.Status != StatusLegacyIncomplete || (p.TotalAmount != nil && *p.TotalAmount > 0) ||
		(p.UnitCost != nil && *p.UnitCost > 0 && roundCents(*p.UnitCost) != roundCents(unitCost)) {
		return prodErrors.ErrPurchaseAlreadyResolved
	}
	if !validMoney(unitCost) || p.Quantity <= 0 {
		return prodErrors.ErrInvalidPurchaseCost
	}
	total := roundCents(unitCost * float64(p.Quantity))
	if !validMoney(total) {
		return prodErrors.ErrInvalidPurchaseCost
	}
	cost := roundCents(unitCost)
	p.UnitCost, p.TotalAmount = &cost, &total
	p.Version++
	p.UpdatedAt = now.UTC()
	return nil
}
