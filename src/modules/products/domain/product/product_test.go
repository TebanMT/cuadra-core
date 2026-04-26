package product_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	productDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/product"
)

func now() time.Time { return time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC) }

func newOK(t *testing.T) *productDomain.Product {
	t.Helper()
	p, err := productDomain.New(uuid.New(), uuid.New(), "Agua Ciel 600ml", 20, 24, 5, nil, nil, now())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return p
}

func TestNew_Validations(t *testing.T) {
	cases := []struct {
		name      string
		pname     string
		price     float64
		stock     int
		stockMin  int
		expectErr error
	}{
		{"name too short", "A", 20, 0, 0, prodErrors.ErrInvalidName},
		{"name blank", "   ", 20, 0, 0, prodErrors.ErrInvalidName},
		{"price zero", "Agua", 0, 0, 0, prodErrors.ErrInvalidPrice},
		{"price negative", "Agua", -1, 0, 0, prodErrors.ErrInvalidPrice},
		{"stock negative", "Agua", 20, -1, 0, prodErrors.ErrInvalidStock},
		{"stock_min negative", "Agua", 20, 0, -1, prodErrors.ErrInvalidStockMin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := productDomain.New(uuid.New(), uuid.New(), tc.pname, tc.price, tc.stock, tc.stockMin, nil, nil, now())
			if !errors.Is(err, tc.expectErr) {
				t.Errorf("err = %v, want %v", err, tc.expectErr)
			}
		})
	}
}

func TestDecrementStock_NoOversell_DA25_2(t *testing.T) {
	p := newOK(t)
	p.Stock = 3
	if err := p.DecrementStock(5, now()); !errors.Is(err, prodErrors.ErrInsufficientStock) {
		t.Errorf("oversell allowed: err = %v", err)
	}
	if p.Stock != 3 {
		t.Errorf("stock mutated on failure: %d", p.Stock)
	}
}

func TestDecrementStock_HappyPath(t *testing.T) {
	p := newOK(t)
	p.Stock = 10
	p.Version = 1
	if err := p.DecrementStock(3, now()); err != nil {
		t.Fatalf("decrement: %v", err)
	}
	if p.Stock != 7 {
		t.Errorf("stock = %d, want 7", p.Stock)
	}
	if p.Version != 2 {
		t.Errorf("version = %d, want 2", p.Version)
	}
}

func TestDecrementStock_RejectsZeroOrNegative(t *testing.T) {
	p := newOK(t)
	for _, qty := range []int{0, -1, -100} {
		if err := p.DecrementStock(qty, now()); !errors.Is(err, prodErrors.ErrInvalidAdjustment) {
			t.Errorf("qty=%d: err = %v", qty, err)
		}
	}
}

func TestIncrementStock(t *testing.T) {
	p := newOK(t)
	p.Stock = 5
	if err := p.IncrementStock(10, now()); err != nil {
		t.Fatalf("inc: %v", err)
	}
	if p.Stock != 15 {
		t.Errorf("stock = %d, want 15", p.Stock)
	}
}

func TestSetStock_CountCorrection(t *testing.T) {
	p := newOK(t)
	p.Stock = 18
	delta, err := p.SetStock(15, now())
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if delta != -3 {
		t.Errorf("delta = %d, want -3", delta)
	}
	if p.Stock != 15 {
		t.Errorf("stock = %d, want 15", p.Stock)
	}
}

func TestSetStock_NoChangeRejected(t *testing.T) {
	p := newOK(t)
	p.Stock = 10
	if _, err := p.SetStock(10, now()); !errors.Is(err, prodErrors.ErrAdjustmentNoChange) {
		t.Errorf("err = %v", err)
	}
}

func TestSetStock_NegativeRejected(t *testing.T) {
	p := newOK(t)
	if _, err := p.SetStock(-1, now()); !errors.Is(err, prodErrors.ErrInvalidStock) {
		t.Errorf("err = %v", err)
	}
}

func TestIsLowStock(t *testing.T) {
	p := newOK(t)
	p.StockMinimum = 5
	p.Stock = 5
	if !p.IsLowStock() {
		t.Errorf("stock==min should be low")
	}
	p.Stock = 6
	if p.IsLowStock() {
		t.Errorf("stock>min should not be low")
	}
	p.Stock = 0
	p.Active = false
	if p.IsLowStock() {
		t.Errorf("inactive products should never trigger low-stock")
	}
}

func TestDeactivate_Idempotent(t *testing.T) {
	p := newOK(t)
	v := p.Version
	p.Deactivate(now())
	if p.Active {
		t.Errorf("still active")
	}
	if p.Version != v+1 {
		t.Errorf("version = %d", p.Version)
	}
	p.Deactivate(now())
	if p.Version != v+1 {
		t.Errorf("re-deactivate bumped version")
	}
}
