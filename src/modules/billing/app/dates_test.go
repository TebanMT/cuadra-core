package app

import (
	"testing"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type fakeGymsForDates struct {
	gymRepo.GymRepository
	gym *gymDomain.Gym
}

func (f *fakeGymsForDates) GetByID(_ sharedDomain.Transaction, _ uuid.UUID) (*gymDomain.Gym, error) {
	return f.gym, nil
}

type datesFakeTx struct{}

func (datesFakeTx) Execute(fn func(tx sharedDomain.Transaction) error) error {
	return fn(datesFakeTx{})
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
