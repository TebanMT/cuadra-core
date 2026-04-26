// Package product holds the Product aggregate of the products BC.
// `stock` is denormalised on the row for fast reads (DA-24.1) — the source of
// truth is the stock_movements event log. Mutations always go through the
// aggregate methods so the aggregate, the movement event and (later) the
// caller's transaction stay aligned.
package product

import (
	"strings"
	"time"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
)

// Product is the catalog item plus a denormalised stock counter. Money is
// kept as float64 here — the SQLite mapper converts to cents at the edge.
type Product struct {
	ID           uuid.UUID
	GymID        uuid.UUID
	Version      int
	Name         string
	Price        float64
	Stock        int
	StockMinimum int
	Category     *string
	ImageURL     *string
	Active       bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
}

// New constructs a Product. `category` and `imageURL` are optional — pass nil
// when not provided. `initialStock` may be 0.
func New(id, gymID uuid.UUID, name string, price float64, initialStock, stockMinimum int,
	category, imageURL *string, now time.Time) (*Product, error) {
	p := &Product{
		ID:        id,
		GymID:     gymID,
		Version:   1,
		Active:    true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := p.applyFields(name, price, stockMinimum, category, imageURL); err != nil {
		return nil, err
	}
	if initialStock < 0 {
		return nil, prodErrors.ErrInvalidStock
	}
	p.Stock = initialStock
	return p, nil
}

// Update mutates the catalog fields (name, price, stock_minimum, category,
// image). Stock is adjusted only via the stock methods below.
func (p *Product) Update(name string, price float64, stockMinimum int,
	category, imageURL *string, now time.Time) error {
	if err := p.applyFields(name, price, stockMinimum, category, imageURL); err != nil {
		return err
	}
	p.Version++
	p.UpdatedAt = now
	return nil
}

// Deactivate is the soft-delete path (DA-23 — products that have been sold
// can't truly be removed because sale_items reference them).
func (p *Product) Deactivate(now time.Time) {
	if !p.Active {
		return
	}
	p.Active = false
	p.Version++
	p.UpdatedAt = now
}

// Reactivate is symmetric.
func (p *Product) Reactivate(now time.Time) {
	if p.Active {
		return
	}
	p.Active = true
	p.Version++
	p.UpdatedAt = now
}

// DecrementStock removes `qty` units. DA-25.2: NO over-sell. Returns
// ErrInsufficientStock when stock - qty < 0. Caller (billing/UC-025) is
// expected to also write a StockMovement of type 'sale'.
func (p *Product) DecrementStock(qty int, now time.Time) error {
	if qty <= 0 {
		return prodErrors.ErrInvalidAdjustment
	}
	if p.Stock-qty < 0 {
		return prodErrors.ErrInsufficientStock
	}
	p.Stock -= qty
	p.Version++
	p.UpdatedAt = now
	return nil
}

// IncrementStock adds `qty` units. Used by UC-024 for `restock` and for refund
// movements that put stock back. `reason` is informational here; the
// StockMovement event log carries the durable record.
func (p *Product) IncrementStock(qty int, now time.Time) error {
	if qty <= 0 {
		return prodErrors.ErrInvalidAdjustment
	}
	p.Stock += qty
	p.Version++
	p.UpdatedAt = now
	return nil
}

// SetStock replaces the absolute count (UC-024 'count_correction' — DA-24.2).
// `newStock` must be ≥ 0 and different from current; returns ErrAdjustmentNoChange
// when no change.
func (p *Product) SetStock(newStock int, now time.Time) (delta int, err error) {
	if newStock < 0 {
		return 0, prodErrors.ErrInvalidStock
	}
	delta = newStock - p.Stock
	if delta == 0 {
		return 0, prodErrors.ErrAdjustmentNoChange
	}
	p.Stock = newStock
	p.Version++
	p.UpdatedAt = now
	return delta, nil
}

// IsLowStock reports whether stock is at or below the configured minimum.
// Used by UC-023 list (low_stock=true filter) and dashboard widgets.
func (p *Product) IsLowStock() bool {
	return p.Active && p.Stock <= p.StockMinimum
}

func (p *Product) applyFields(name string, price float64, stockMinimum int,
	category, imageURL *string) error {
	name = strings.TrimSpace(name)
	if len(name) < 2 || len(name) > 100 {
		return prodErrors.ErrInvalidName
	}
	if price <= 0 {
		return prodErrors.ErrInvalidPrice
	}
	if stockMinimum < 0 {
		return prodErrors.ErrInvalidStockMin
	}
	if category != nil {
		c := strings.TrimSpace(*category)
		if c == "" {
			category = nil
		} else {
			category = &c
		}
	}
	if imageURL != nil {
		u := strings.TrimSpace(*imageURL)
		if u == "" {
			imageURL = nil
		} else {
			imageURL = &u
		}
	}
	p.Name = name
	p.Price = price
	p.StockMinimum = stockMinimum
	p.Category = category
	p.ImageURL = imageURL
	return nil
}

// Validator is the chain interface for create-time validation. Mirrors the
// pattern used by the membership_type aggregate so the products BC stays
// syntactically familiar.
type Validator interface {
	Validate(p *Product) error
}

type nameValidator struct{ Next Validator }

func (v *nameValidator) Validate(p *Product) error {
	if len(strings.TrimSpace(p.Name)) < 2 || len(p.Name) > 100 {
		return prodErrors.ErrInvalidName
	}
	if v.Next != nil {
		return v.Next.Validate(p)
	}
	return nil
}

type priceValidator struct{ Next Validator }

func (v *priceValidator) Validate(p *Product) error {
	if p.Price <= 0 {
		return prodErrors.ErrInvalidPrice
	}
	if v.Next != nil {
		return v.Next.Validate(p)
	}
	return nil
}

type stockValidator struct{ Next Validator }

func (v *stockValidator) Validate(p *Product) error {
	if p.Stock < 0 {
		return prodErrors.ErrInvalidStock
	}
	if p.StockMinimum < 0 {
		return prodErrors.ErrInvalidStockMin
	}
	if v.Next != nil {
		return v.Next.Validate(p)
	}
	return nil
}

func BuildValidatorChain() Validator {
	return &nameValidator{Next: &priceValidator{Next: &stockValidator{}}}
}
