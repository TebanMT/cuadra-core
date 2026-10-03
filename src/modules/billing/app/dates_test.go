package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	paymentDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/payment"
	billingRepo "github.com/cuadra/cuadra-core/src/modules/billing/domain/repository"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type fakeGymsForDates struct {
	gymRepo.GymRepository
	gym *gymDomain.Gym
	err error
}

func (f *fakeGymsForDates) GetByID(_ sharedDomain.Transaction, _ uuid.UUID) (*gymDomain.Gym, error) {
	return f.gym, f.err
}

type datesFakeTx struct{}

func (datesFakeTx) Execute(fn func(tx sharedDomain.Transaction) error) error {
	return fn(datesFakeTx{})
}

type datesFakeUoW struct{}

func (datesFakeUoW) Begin(context.Context) (sharedDomain.Transaction, error) {
	return datesFakeTx{}, nil
}
func (datesFakeUoW) Commit(sharedDomain.Transaction) error   { return nil }
func (datesFakeUoW) Rollback(sharedDomain.Transaction) error { return nil }
func (datesFakeUoW) Query(context.Context) (sharedDomain.Transaction, error) {
	return datesFakeTx{}, nil
}
func (datesFakeUoW) Command(_ context.Context, fn func(sharedDomain.Transaction) error) error {
	return fn(datesFakeTx{})
}

type monetaryPaymentSpy struct {
	billingRepo.PaymentRepository
	touched bool
}

func (r *monetaryPaymentSpy) GetByID(sharedDomain.Transaction, uuid.UUID) (*paymentDomain.Payment, error) {
	r.touched = true
	return nil, billingErrors.ErrPaymentNotFound
}

// Pin del bug del cobro nocturno: a las 10 PM de CDMX (04:00 UTC del día
// siguiente) el default de PaymentDate debe ser el día LOCAL en curso —
// con el anclaje UTC anterior, la renovación vencía un día después y el
// pago caía en la caja del día equivocado.
func TestGymLocalPaymentDate_CobroNocturnoNoCruzaDeDia(t *testing.T) {
	gyms := &fakeGymsForDates{gym: &gymDomain.Gym{
		ID: uuid.New(), Timezone: "America/Mexico_City",
	}}
	now := time.Date(2026, 7, 7, 4, 0, 0, 0, time.UTC) // 6-jul 10 PM CDMX

	got := gymLocalPaymentDate(datesFakeTx{}, gyms, uuid.New(), now)
	want := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("PaymentDate default = %v, want día local %v", got, want)
	}
}

func TestGymLocalPaymentDate_SinRepoCaeAlDiaUTC(t *testing.T) {
	now := time.Date(2026, 7, 7, 4, 0, 0, 0, time.UTC)
	got := gymLocalPaymentDate(datesFakeTx{}, nil, uuid.New(), now)
	want := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("sin Gyms cableado = %v, want fallback UTC %v", got, want)
	}
}

func TestGymLocalPaymentDate_TzVaciaCaeAlDiaUTC(t *testing.T) {
	gyms := &fakeGymsForDates{gym: &gymDomain.Gym{ID: uuid.New(), Timezone: ""}}
	now := time.Date(2026, 7, 7, 4, 0, 0, 0, time.UTC)
	got := gymLocalPaymentDate(datesFakeTx{}, gyms, uuid.New(), now)
	want := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("tz vacía = %v, want fallback UTC %v", got, want)
	}
}

func TestResolveMonetaryDate_CDMXNocturnalMatrix(t *testing.T) {
	gyms := &fakeGymsForDates{gym: &gymDomain.Gym{
		ID: uuid.New(), Timezone: "America/Mexico_City",
	}}
	// 25-ago 04:30 UTC todavía es 24-ago 22:30 en CDMX.
	now := time.Date(2026, 8, 25, 4, 30, 0, 0, time.UTC)
	localToday := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		requested time.Time
		want      time.Time
		wantError bool
	}{
		{name: "empty defaults to local today", want: localToday},
		{name: "explicit local today", requested: localToday, want: localToday},
		{name: "backdate remains valid", requested: localToday.AddDate(0, 0, -1), want: localToday.AddDate(0, 0, -1)},
		{name: "UTC tomorrow is still future for gym", requested: localToday.AddDate(0, 0, 1), wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMonetaryDate(datesFakeTx{}, gyms, gyms.gym.ID, tc.requested, now)
			if tc.wantError {
				if !errors.Is(err, billingErrors.ErrPaymentDateInvalid) {
					t.Fatalf("error = %v, want ErrPaymentDateInvalid", err)
				}
				return
			}
			if err != nil || !got.Equal(tc.want) {
				t.Fatalf("date = %v, err = %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestResolveMonetaryDate_ConfiguredGymCalendarFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 25, 4, 30, 0, 0, time.UTC)
	gymID := uuid.New()
	repositoryFailure := errors.New("gym database unavailable")
	tests := []struct {
		name string
		repo *fakeGymsForDates
		want error
	}{
		{name: "repository failure", repo: &fakeGymsForDates{err: repositoryFailure}, want: repositoryFailure},
		{name: "missing gym", repo: &fakeGymsForDates{}},
		{name: "missing timezone", repo: &fakeGymsForDates{gym: &gymDomain.Gym{ID: gymID}}},
		{name: "invalid timezone", repo: &fakeGymsForDates{gym: &gymDomain.Gym{ID: gymID, Timezone: "Mars/Olympus_Mons"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveMonetaryDate(datesFakeTx{}, tc.repo, gymID, time.Time{}, now)
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

func TestMonetaryCommand_CalendarFailurePrecedesIdempotencyAndMutation(t *testing.T) {
	repositoryFailure := errors.New("gym database unavailable")
	payments := &monetaryPaymentSpy{}
	uc := NewRegisterOtherIncome(payments, nil, datesFakeUoW{}, nil).
		WithGyms(&fakeGymsForDates{err: repositoryFailure})
	_, err := uc.Execute(context.Background(), RegisterOtherIncomeInput{
		GymID: uuid.New(), ActorUserID: uuid.New(), Amount: 100,
		Method: "transfer", Description: "Venta de equipo usado", IdempotencyKey: "strict-calendar",
	})
	if !errors.Is(err, repositoryFailure) {
		t.Fatalf("error=%v, want calendar repository failure", err)
	}
	if payments.touched {
		t.Fatal("payment/idempotency repository touched after calendar failure")
	}
}

func TestValidateMonetaryChronology_SameDayAndBackdate(t *testing.T) {
	source := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	if err := validateMonetaryChronology(source, source); err != nil {
		t.Fatalf("same-day child must be valid: %v", err)
	}
	if err := validateMonetaryChronology(source.AddDate(0, 0, 1), source); err != nil {
		t.Fatalf("later child must be valid: %v", err)
	}
	if err := validateMonetaryChronology(source.AddDate(0, 0, -1), source); !errors.Is(err, billingErrors.ErrPaymentDateInvalid) {
		t.Fatalf("child before source error = %v, want ErrPaymentDateInvalid", err)
	}
}
