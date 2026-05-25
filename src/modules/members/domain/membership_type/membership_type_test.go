package membership_type_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	mt "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
)

func TestNew(t *testing.T) {
	now := time.Now().UTC()
	gymID := uuid.New()

	if _, err := mt.New(uuid.New(), gymID, "Mensual", 500, 30, nil, 0, 0, "", now); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if _, err := mt.New(uuid.New(), gymID, "", 500, 30, nil, 0, 0, "", now); err == nil {
		t.Errorf("empty name should fail")
	}
	if _, err := mt.New(uuid.New(), gymID, "Mensual", 0, 30, nil, 0, 0, "", now); err == nil {
		t.Errorf("zero price should fail")
	}
	if _, err := mt.New(uuid.New(), gymID, "Mensual", 500, 0, nil, 0, 0, "", now); err == nil {
		t.Errorf("zero duration should fail")
	}
	// Maintenance fee with no frequency should fail.
	if _, err := mt.New(uuid.New(), gymID, "Anual", 5000, 365, nil, 0, 100, "", now); err == nil {
		t.Errorf("maintenance fee without frequency should fail")
	}
	// Maintenance fee with valid frequency.
	x, err := mt.New(uuid.New(), gymID, "Anual", 5000, 365, nil, 0, 100, "monthly", now)
	if err != nil {
		t.Fatalf("monthly maintenance: %v", err)
	}
	if x.MaintenanceFrequency == nil || *x.MaintenanceFrequency != "monthly" {
		t.Errorf("frequency = %v", x.MaintenanceFrequency)
	}

	// Preset mensual (durationDays=30 + durationMonths=1) → DurationMonths
	// queda persistido como puntero a 1. El cálculo del expiry vive en el
	// dominio Membership; este test sólo valida que applyFields acepta
	// el dúo (días, meses).
	one := 1
	plan, err := mt.New(uuid.New(), gymID, "Mensual", 500, 30, &one, 0, 0, "", now)
	if err != nil {
		t.Fatalf("preset mensual: %v", err)
	}
	if plan.DurationMonths == nil || *plan.DurationMonths != 1 {
		t.Errorf("DurationMonths esperado 1, got %v", plan.DurationMonths)
	}

	// duration_months fuera de rango (61+) debería fallar.
	too := 61
	if _, err := mt.New(uuid.New(), gymID, "Loco", 500, 30, &too, 0, 0, "", now); err == nil {
		t.Errorf("DurationMonths=61 debería fallar")
	}
}
