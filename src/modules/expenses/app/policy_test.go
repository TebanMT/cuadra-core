package app

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	expErrors "github.com/cuadra/cuadra-core/src/modules/expenses/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

type policyTx struct{}

func (policyTx) Execute(fn func(shared.Transaction) error) error { return fn(policyTx{}) }

type policyGymRepo struct {
	zone    string
	err     error
	missing bool
}

func (r policyGymRepo) GetByID(shared.Transaction, uuid.UUID) (*gym.Gym, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.missing {
		return nil, nil
	}
	return &gym.Gym{Timezone: r.zone}, nil
}
func (policyGymRepo) Create(shared.Transaction, *gym.Gym) (*gym.Gym, error) { panic("not used") }
func (policyGymRepo) Update(shared.Transaction, *gym.Gym) (*gym.Gym, error) { panic("not used") }
func (policyGymRepo) HasMembershipType(shared.Transaction, uuid.UUID) (bool, error) {
	panic("not used")
}
func (policyGymRepo) ExistsByWhatsApp(shared.Transaction, string, uuid.UUID) (bool, error) {
	panic("not used")
}

func TestValidatePaidDateUsesGymLocalCalendarDay(t *testing.T) {
	// 05:30 UTC is still 23:30 of Aug 13 in Mexico City. Aug 14 must be
	// rejected there, while it is already Aug 14 in Tokyo.
	now := time.Date(2026, 8, 14, 5, 30, 0, 0, time.UTC)
	paidOn := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)

	mexico := OperationalPolicy{Gyms: policyGymRepo{zone: "America/Mexico_City"}}
	if err := mexico.validatePaidDate(policyTx{}, uuid.New(), paidOn, now); !errors.Is(err, expErrors.ErrFutureDate) {
		t.Fatalf("Mexico City error=%v, want future date", err)
	}

	tokyo := OperationalPolicy{Gyms: policyGymRepo{zone: "Asia/Tokyo"}}
	if err := tokyo.validatePaidDate(policyTx{}, uuid.New(), paidOn, now); err != nil {
		t.Fatalf("Tokyo same local date rejected: %v", err)
	}
}

func TestValidatePaidDateDoesNotHideTimezoneRepositoryFailure(t *testing.T) {
	want := errors.New("database unavailable")
	policy := OperationalPolicy{Gyms: policyGymRepo{err: want}}
	err := policy.validatePaidDate(policyTx{}, uuid.New(), time.Now(), time.Now())
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("error=%v, want wrapped repository failure", err)
	}
}

func TestValidatePaidDateConfiguredGymFailsClosedWhenCalendarIsMissingOrInvalid(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	for _, repo := range []policyGymRepo{
		{missing: true},
		{zone: ""},
		{zone: "Mars/Olympus_Mons"},
	} {
		policy := OperationalPolicy{Gyms: repo}
		if err := policy.validatePaidDate(policyTx{}, uuid.New(), now, now); err == nil {
			t.Fatalf("configured invalid gym calendar %+v was accepted", repo)
		}
	}
}
