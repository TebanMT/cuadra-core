package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	prodErrors "github.com/cuadra/cuadra-core/src/modules/products/domain/errors"
	purchaseDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/purchase"
	prodRepo "github.com/cuadra/cuadra-core/src/modules/products/domain/repository"
	stockMovementDomain "github.com/cuadra/cuadra-core/src/modules/products/domain/stockmovement"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type purchaseDateGyms struct {
	gymRepo.GymRepository
	gym *gymDomain.Gym
	err error
}

func (f *purchaseDateGyms) GetByID(sharedDomain.Transaction, uuid.UUID) (*gymDomain.Gym, error) {
	return f.gym, f.err
}

type purchaseDateTx struct{}

func (purchaseDateTx) Execute(fn func(sharedDomain.Transaction) error) error {
	return fn(purchaseDateTx{})
}

type purchaseDateUoW struct{}

func (purchaseDateUoW) Begin(context.Context) (sharedDomain.Transaction, error) {
	return purchaseDateTx{}, nil
}
func (purchaseDateUoW) Commit(sharedDomain.Transaction) error   { return nil }
func (purchaseDateUoW) Rollback(sharedDomain.Transaction) error { return nil }
func (purchaseDateUoW) Query(context.Context) (sharedDomain.Transaction, error) {
	return purchaseDateTx{}, nil
}
func (purchaseDateUoW) Command(_ context.Context, fn func(sharedDomain.Transaction) error) error {
	return fn(purchaseDateTx{})
}

type purchaseDateStockSpy struct {
	prodRepo.StockMovementRepository
	touched bool
}

func (r *purchaseDateStockSpy) GetByIdempotencyKey(sharedDomain.Transaction, uuid.UUID, string) (*stockMovementDomain.StockMovement, error) {
	r.touched = true
	return nil, nil
}

type purchaseDatePurchaseRepo struct {
	prodRepo.InventoryPurchaseRepository
}

func TestResolvePurchasePaidOn_CDMXNocturnalMatrix(t *testing.T) {
	gyms := &purchaseDateGyms{gym: &gymDomain.Gym{ID: uuid.New(), Timezone: "America/Mexico_City"}}
	// 25-ago 04:30 UTC todavía es 24-ago 22:30 en el gimnasio.
	now := time.Date(2026, 8, 25, 4, 30, 0, 0, time.UTC)
	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		requested time.Time
		want      time.Time
		wantError bool
	}{
		{name: "local today", requested: today, want: today},
		{name: "backdate", requested: today.AddDate(0, 0, -1), want: today.AddDate(0, 0, -1)},
		{name: "UTC tomorrow is future locally", requested: today.AddDate(0, 0, 1), wantError: true},
		{name: "missing paid date", wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolvePurchasePaidOn(nil, gyms, gyms.gym.ID, tc.requested, now)
			if tc.wantError {
				if !errors.Is(err, prodErrors.ErrPurchaseDateInvalid) {
					t.Fatalf("error=%v, want ErrPurchaseDateInvalid", err)
				}
				return
			}
			if err != nil || !got.Equal(tc.want) {
				t.Fatalf("date=%v err=%v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestResolvePurchasePaidOn_ConfiguredGymCalendarFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 25, 4, 30, 0, 0, time.UTC)
	gymID := uuid.New()
	repositoryFailure := errors.New("gym database unavailable")
	tests := []struct {
		name string
		repo *purchaseDateGyms
		want error
	}{
		{name: "repository failure", repo: &purchaseDateGyms{err: repositoryFailure}, want: repositoryFailure},
		{name: "missing gym", repo: &purchaseDateGyms{}},
		{name: "missing timezone", repo: &purchaseDateGyms{gym: &gymDomain.Gym{ID: gymID}}},
		{name: "invalid timezone", repo: &purchaseDateGyms{gym: &gymDomain.Gym{ID: gymID, Timezone: "Mars/Olympus_Mons"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolvePurchasePaidOn(purchaseDateTx{}, tc.repo, gymID,
				time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), now)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("error=%v, want strict calendar failure", err)
			}
			var custom sharedDomain.CustomError
			if !errors.As(err, &custom) || custom.ErrorCode != sharedDomain.CodeUnexpected {
				t.Fatalf("error=%#v, want unexpected classification", err)
			}
		})
	}
}

func TestPaidInventoryPurchase_CalendarFailurePrecedesIdempotencyAndMutation(t *testing.T) {
	repositoryFailure := errors.New("gym database unavailable")
	stock := &purchaseDateStockSpy{}
	uc := (&AdjustStock{
		StockMovements: stock,
		Purchases:      &purchaseDatePurchaseRepo{},
		Gyms:           &purchaseDateGyms{err: repositoryFailure},
		UoW:            purchaseDateUoW{},
	})
	isPurchase := true
	cost := 5.0
	paidOn := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	_, err := uc.Execute(context.Background(), AdjustStockInput{
		GymID: uuid.New(), ActorUserID: uuid.New(), ActorRole: "owner", ProductID: uuid.New(),
		MovementType: stockMovementDomain.TypeRestock, Quantity: 4, Cost: &cost,
		IsPurchase: &isPurchase, PurchaseStatus: purchaseDomain.StatusPaid, PaidOn: &paidOn,
		PaymentMethod: purchaseDomain.MethodTransfer, PaidFrom: purchaseDomain.PaidFromGymFund,
		IdempotencyKey: "strict-purchase-calendar",
	})
	if !errors.Is(err, repositoryFailure) {
		t.Fatalf("error=%v, want calendar repository failure", err)
	}
	if stock.touched {
		t.Fatal("stock/idempotency repository touched after calendar failure")
	}
}
