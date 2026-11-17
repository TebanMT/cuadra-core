package membership_test

// UC-048 — reglas de dominio del cambio de plan a mitad de ciclo. La regla
// central (DA-48.2): la fecha de vencimiento se CONSERVA exacta; el prorrateo
// existe precisamente para no mover el día de pago del socio.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
)

func mustTypePriced(t *testing.T, name string, price float64, durationDays int) *mtDomain.MembershipType {
	t.Helper()
	now := time.Now().UTC()
	mt, err := mtDomain.New(uuid.New(), uuid.New(), name, price, durationDays, nil, 0, 0, "", now)
	if err != nil {
		t.Fatalf("type %s: %v", name, err)
	}
	return mt
}

// activeMembershipFor construye la membresía vigente de los tests: plan
// "Gym" $450, 1-abr → 1-may (30 días corridos).
func activeMembershipFor(t *testing.T, mt *mtDomain.MembershipType) *membership.Membership {
	t.Helper()
	now := time.Now().UTC()
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	return membership.New(uuid.New(), mt.GymID, uuid.New(), mt, start, now)
}

func TestChangePlan_PreservesExpiryExactly(t *testing.T) {
	now := time.Now().UTC()
	oldType := mustTypePriced(t, "Gym", 450, 30)
	current := activeMembershipFor(t, oldType) // vence 1-may
	// Tipo nuevo con otra duración a propósito: las fechas reales gobiernan
	// la vigencia, el snapshot sólo congela el plan para la siguiente
	// renovación.
	newType := mustTypePriced(t, "Gym + Clases", 700, 90)
	today := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)

	next, err := current.ChangePlan(uuid.New(), newType, today, now)
	if err != nil {
		t.Fatalf("change plan: %v", err)
	}
	if !next.StartDate.Equal(today) {
		t.Errorf("start = %v, want hoy (%v)", next.StartDate, today)
	}
	wantExpiry := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if next.ExpiryDate == nil || !next.ExpiryDate.Equal(wantExpiry) {
		t.Errorf("expiry = %v, want CONSERVADA %v (DA-48.2)", next.ExpiryDate, wantExpiry)
	}
	if next.TypeNameSnapshot != "Gym + Clases" || next.PriceSnapshot != 700 || next.DurationDaysSnapshot != 90 {
		t.Errorf("snapshot del tipo nuevo mal congelado: %+v", next)
	}
	if next.MembershipTypeID != newType.ID {
		t.Errorf("type id = %v, want %v", next.MembershipTypeID, newType.ID)
	}
	if next.Status != membership.StatusActive || next.Version != 1 {
		t.Errorf("status/version = %s/%d, want active/1", next.Status, next.Version)
	}
	if next.MemberID != current.MemberID || next.GymID != current.GymID {
		t.Errorf("member/gym no se conservaron")
	}
	// ChangePlan NO muta la actual — el caller la marca replaced.
	if current.Status != membership.StatusActive || current.ReplacedBy != nil {
		t.Errorf("la membresía actual fue mutada: %+v", current)
	}
}

func TestChangePlan_LateralSamePriceAllowed(t *testing.T) {
	now := time.Now().UTC()
	oldType := mustTypePriced(t, "Gym matutino", 450, 30)
	current := activeMembershipFor(t, oldType)
	lateral := mustTypePriced(t, "Gym vespertino", 450, 30)
	today := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)

	next, err := current.ChangePlan(uuid.New(), lateral, today, now)
	if err != nil {
		t.Fatalf("cambio lateral (mismo precio) debió permitirse: %v", err)
	}
	if next.PriceSnapshot != 450 || next.TypeNameSnapshot != "Gym vespertino" {
		t.Errorf("snapshot lateral: %+v", next)
	}
}

func TestChangePlan_OnExpiryDayAllowed(t *testing.T) {
	// Hoy cuenta como restante: el socio que vence HOY todavía puede cambiar.
	now := time.Now().UTC()
	oldType := mustTypePriced(t, "Gym", 450, 30)
	current := activeMembershipFor(t, oldType) // vence 1-may
	newType := mustTypePriced(t, "Gym + Clases", 700, 30)
	today := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	if _, err := current.ChangePlan(uuid.New(), newType, today, now); err != nil {
		t.Fatalf("el día del vencimiento aún no está vencida: %v", err)
	}
}

func TestChangePlan_ExpiredRejected(t *testing.T) {
	now := time.Now().UTC()
	oldType := mustTypePriced(t, "Gym", 450, 30)
	current := activeMembershipFor(t, oldType) // vence 1-may
	newType := mustTypePriced(t, "Gym + Clases", 700, 30)
	today := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)

	_, err := current.ChangePlan(uuid.New(), newType, today, now)
	if !errors.Is(err, memErrors.ErrPlanChangeExpired) {
		t.Fatalf("err = %v, want ErrPlanChangeExpired (vencida → UC-018)", err)
	}
}

func TestChangePlan_DowngradeRejected(t *testing.T) {
	now := time.Now().UTC()
	oldType := mustTypePriced(t, "Gym + Clases", 700, 30)
	current := activeMembershipFor(t, oldType)
	cheaper := mustTypePriced(t, "Gym", 450, 30)
	today := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)

	_, err := current.ChangePlan(uuid.New(), cheaper, today, now)
	if !errors.Is(err, memErrors.ErrPlanChangeDowngrade) {
		t.Fatalf("err = %v, want ErrPlanChangeDowngrade (DA-48.1: downgrade al renovar)", err)
	}
}

func TestChangePlan_SameTypeRejected(t *testing.T) {
	now := time.Now().UTC()
	oldType := mustTypePriced(t, "Gym", 450, 30)
	current := activeMembershipFor(t, oldType)
	today := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)

	_, err := current.ChangePlan(uuid.New(), oldType, today, now)
	if !errors.Is(err, memErrors.ErrPlanChangeSameType) {
		t.Fatalf("err = %v, want ErrPlanChangeSameType", err)
	}
}

func TestChangePlan_PendingPaymentRejected(t *testing.T) {
	now := time.Now().UTC()
	oldType := mustTypePriced(t, "Gym", 450, 30)
	start := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	pending := membership.NewPendingPayment(uuid.New(), oldType.GymID, uuid.New(), oldType, start, now)
	newType := mustTypePriced(t, "Gym + Clases", 700, 30)
	today := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)

	_, err := pending.ChangePlan(uuid.New(), newType, today, now)
	if !errors.Is(err, memErrors.ErrNoActiveMembership) {
		t.Fatalf("err = %v, want ErrNoActiveMembership (pending → UC-018 activa con el plan nuevo)", err)
	}
}
