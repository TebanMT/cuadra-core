// Package sale holds the Sale aggregate + SaleItem value object. A Sale is
// linked 1-to-1 with a Payment of concept='product' (UC-025). Each SaleItem
// snapshots `product_name` and `unit_price` at the moment of the sale so
// historical receipts stay correct even if the catalog changes later.
package sale

import (
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
)

// SaleItem is the value object inside a Sale. It is immutable once persisted.
// `LineTotal` MUST equal UnitPriceSnapshot * Quantity (rounded to cents).
type SaleItem struct {
	ID                  uuid.UUID
	GymID               uuid.UUID
	Version             int
	SaleID              uuid.UUID
	ProductID           uuid.UUID
	ProductNameSnapshot string
	UnitPriceSnapshot   float64
	Quantity            int
	LineTotal           float64
	CreatedAt           time.Time
	UpdatedAt           time.Time
	DeletedAt           *time.Time
}

// NewItem constructs a SaleItem after validating the snapshot fields. The
// caller (RegisterSale use case) provides the snapshot from the Product it
// just decremented.
func NewItem(id, gymID, saleID, productID uuid.UUID,
	productName string, unitPrice float64, quantity int, now time.Time) (*SaleItem, error) {
	productName = strings.TrimSpace(productName)
	if productName == "" {
		return nil, billingErrors.ErrSaleItemNameRequired
	}
	if unitPrice <= 0 {
		return nil, billingErrors.ErrSaleItemPriceInvalid
	}
	if quantity <= 0 {
		return nil, billingErrors.ErrSaleItemQuantityInvalid
	}
	return &SaleItem{
		ID:                  id,
		GymID:               gymID,
		Version:             1,
		SaleID:              saleID,
		ProductID:           productID,
		ProductNameSnapshot: productName,
		UnitPriceSnapshot:   roundCents(unitPrice),
		Quantity:            quantity,
		LineTotal:           roundCents(unitPrice * float64(quantity)),
		CreatedAt:           now,
		UpdatedAt:           now,
	}, nil
}

// Sale is the aggregate root. It owns its line items by id, but the items are
// persisted as their own table (sale_items) so reports can run cheap GROUP BYs.
type Sale struct {
	ID        uuid.UUID
	GymID     uuid.UUID
	Version   int
	PaymentID uuid.UUID
	MemberID  *uuid.UUID
	Subtotal  float64
	Discount  float64
	Total     float64
	Items     []*SaleItem
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// NewSaleInput is the constructor input. `Items` is the list of (product, qty,
// snapshot price/name) tuples. The use case is responsible for having
// decremented stock on each underlying Product before calling NewSale.
type NewSaleInput struct {
	GymID     uuid.UUID
	PaymentID uuid.UUID
	MemberID  *uuid.UUID
	Discount  float64
	Items     []ItemInput
}

// ItemInput carries the fields needed to build a single SaleItem.
type ItemInput struct {
	ProductID           uuid.UUID
	ProductNameSnapshot string
	UnitPriceSnapshot   float64
	Quantity            int
}

// NewSale builds the Sale + its SaleItems atomically. Validates carrito non-empty
// (DA-25.4) and computes subtotal / total. Discount may be 0; it is bounded by
// subtotal — caller can leave at 0 in MVP.
func NewSale(id uuid.UUID, in NewSaleInput, now time.Time) (*Sale, error) {
	if len(in.Items) == 0 {
		return nil, billingErrors.ErrSaleEmpty
	}
	if in.Discount < 0 {
		return nil, billingErrors.ErrAmountInvalid
	}
	items := make([]*SaleItem, 0, len(in.Items))
	var subtotal float64
	for _, it := range in.Items {
		si, err := NewItem(uuid.New(), in.GymID, id, it.ProductID,
			it.ProductNameSnapshot, it.UnitPriceSnapshot, it.Quantity, now)
		if err != nil {
			return nil, err
		}
		items = append(items, si)
		subtotal += si.LineTotal
	}
	subtotal = roundCents(subtotal)
	if in.Discount >= subtotal {
		return nil, billingErrors.ErrDiscountTooLarge
	}
	total := roundCents(subtotal - in.Discount)
	return &Sale{
		ID:        id,
		GymID:     in.GymID,
		Version:   1,
		PaymentID: in.PaymentID,
		MemberID:  in.MemberID,
		Subtotal:  subtotal,
		Discount:  roundCents(in.Discount),
		Total:     total,
		Items:     items,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// roundCents matches payment.roundCents — duplicated here so the sale package
// stays self-contained (no import cycle into payment).
func roundCents(v float64) float64 {
	if v >= 0 {
		return float64(int64(v*100+0.5)) / 100
	}
	return float64(int64(v*100-0.5)) / 100
}
