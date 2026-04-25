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

	if _, err := mt.New(uuid.New(), gymID, "Mensual", 500, 30, 0, 0, "", now); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if _, err := mt.New(uuid.New(), gymID, "", 500, 30, 0, 0, "", now); err == nil {
		t.Errorf("empty name should fail")
	}
	if _, err := mt.New(uuid.New(), gymID, "Mensual", 0, 30, 0, 0, "", now); err == nil {
		t.Errorf("zero price should fail")
	}
	if _, err := mt.New(uuid.New(), gymID, "Mensual", 500, 0, 0, 0, "", now); err == nil {
		t.Errorf("zero duration should fail")
	}
	// Maintenance fee with no frequency should fail.
	if _, err := mt.New(uuid.New(), gymID, "Anual", 5000, 365, 0, 100, "", now); err == nil {
		t.Errorf("maintenance fee without frequency should fail")
	}
	// Maintenance fee with valid frequency.
	x, err := mt.New(uuid.New(), gymID, "Anual", 5000, 365, 0, 100, "monthly", now)
	if err != nil {
		t.Fatalf("monthly maintenance: %v", err)
	}
	if x.MaintenanceFrequency == nil || *x.MaintenanceFrequency != "monthly" {
		t.Errorf("frequency = %v", x.MaintenanceFrequency)
	}
}
