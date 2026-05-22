package gym_test

import (
	"testing"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	"github.com/cuadra/cuadra-core/src/shared/runtime"
)

// Estos tests cubren el bypass TINTA_MODE=dev de CanAccessPlusFeatures.
// Importante: IsPlusPlan e IsPaidPlan NO deben verse afectados — el SKU
// real del gym sigue siendo honesto incluso en dev.

func TestCanAccessPlusFeatures_ProductionRespectsSKU(t *testing.T) {
	runtime.SetForTest(t, runtime.ModeProduction)
	if gymDomain.CanAccessPlusFeatures(gymDomain.PlanTrial) {
		t.Fatal("production: trial debería NO ver Plus")
	}
	if gymDomain.CanAccessPlusFeatures(gymDomain.PlanStandardMonthly) {
		t.Fatal("production: standard_monthly debería NO ver Plus")
	}
	if !gymDomain.CanAccessPlusFeatures(gymDomain.PlanPlusMonthly) {
		t.Fatal("production: plus_monthly debería ver Plus")
	}
}

func TestCanAccessPlusFeatures_TestModeRespectsSKU(t *testing.T) {
	runtime.SetForTest(t, runtime.ModeTest)
	if gymDomain.CanAccessPlusFeatures(gymDomain.PlanTrial) {
		t.Fatal("test: trial debería NO ver Plus (idéntico a production)")
	}
	if gymDomain.CanAccessPlusFeatures(gymDomain.PlanStandardMonthly) {
		t.Fatal("test: standard_monthly debería NO ver Plus")
	}
	if !gymDomain.CanAccessPlusFeatures(gymDomain.PlanPlusMonthly) {
		t.Fatal("test: plus_monthly debería ver Plus")
	}
}

func TestCanAccessPlusFeatures_DevBypassesGate(t *testing.T) {
	runtime.SetForTest(t, runtime.ModeDev)
	for _, plan := range []string{
		gymDomain.PlanTrial,
		gymDomain.PlanStandardMonthly,
		gymDomain.PlanStandardAnnual,
		gymDomain.PlanPlusMonthly,
		gymDomain.PlanPlusAnnual,
	} {
		if !gymDomain.CanAccessPlusFeatures(plan) {
			t.Fatalf("dev: plan %q debería ver Plus (bypass)", plan)
		}
	}
}

// El bypass NO debe leakear a IsPlusPlan/IsPaidPlan — billing y reporting
// interno siguen viendo el SKU real del gym aunque estemos en dev mode.
func TestDevBypassDoesNotLeakToSKUHelpers(t *testing.T) {
	runtime.SetForTest(t, runtime.ModeDev)
	if gymDomain.IsPlusPlan(gymDomain.PlanTrial) {
		t.Fatal("dev: IsPlusPlan(trial) debe seguir devolviendo false")
	}
	if gymDomain.IsPaidPlan(gymDomain.PlanTrial) {
		t.Fatal("dev: IsPaidPlan(trial) debe seguir devolviendo false")
	}
	if gymDomain.IsPlusPlan(gymDomain.PlanStandardMonthly) {
		t.Fatal("dev: IsPlusPlan(standard_monthly) debe seguir devolviendo false")
	}
}
