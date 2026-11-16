package gym

import (
	"testing"
	"time"
)

// Oferta vigente (16 nov 2026): la prueba y Standard traen todo lo que existe;
// Plus sólo conserva el número propio de WhatsApp y los envíos sin tope.
var standardFeatures = []Feature{
	FeatureDailyOperations, FeatureRecurringExpenses, FeatureAnalytics,
	FeatureReportExports, FeatureProductMargins, FeaturePromotionsROI,
	FeatureAuditLog, FeatureBranding, FeatureFiscalDetails,
	FeatureAccessHardware, FeatureHeldMessages, FeatureExtraOperators,
}

var plusOnlyFeatures = []Feature{FeatureOwnWhatsApp, FeatureBroadcasts}

func TestStandardAndTrialIncludeEveryBuiltFeature(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	for _, plan := range []string{PlanTrial, PlanStandardMonthly, PlanStandardAnnual} {
		g := &Gym{SubscriptionPlan: plan}
		for _, f := range standardFeatures {
			if tier, ok := MinimumTier(f); !ok || tier != TierStandard {
				t.Errorf("MinimumTier(%s) = %q, %v; want standard", f, tier, ok)
			}
			if !g.CanAccessFeature(f) {
				t.Errorf("%s sin grant debe tener %s", plan, f)
			}
		}
		for _, f := range plusOnlyFeatures {
			if g.CanAccessFeature(f) {
				t.Errorf("%s sin grant no debe tener %s", plan, f)
			}
		}
	}
	// Un plan desconocido o vacío no recibe nada.
	if (&Gym{SubscriptionPlan: ""}).CanAccessFeature(FeatureAnalytics) {
		t.Fatal("plan vacío debe fallar cerrado")
	}
}

func TestLaunchAccessDoesNotChangeBillingOrExpiration(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	for _, plan := range []string{PlanTrial, PlanStandardMonthly, PlanStandardAnnual, PlanPlusMonthly, PlanPlusAnnual} {
		for _, grant := range []bool{false, true} {
			g := &Gym{SubscriptionPlan: plan, AllFeaturesAccess: grant, SubscriptionStatus: StatusCancelled}
			for _, f := range standardFeatures {
				if !g.CanAccessFeature(f) {
					t.Errorf("%s grant=%v feature=%s: Standard feature denied", plan, grant, f)
				}
			}
			for _, f := range plusOnlyFeatures {
				if got := g.CanAccessFeature(f); got != (grant || IsPlusPlan(plan)) {
					t.Errorf("%s grant=%v feature=%s: %v", plan, grant, f, got)
				}
			}
			if g.CanAccessFeature("unknown_future_feature") {
				t.Fatal("unknown feature must fail closed")
			}
			// Una prueba cancelada queda en solo lectura (UC-002); un plan de
			// pago cancelado, bloqueado. El grant no cambia ninguna de las dos.
			now := time.Now()
			hardBlocked := plan != PlanTrial
			if g.SubscriptionPlan != plan || g.HasActiveAccess(now) || g.IsAccessHardBlocked(now) != hardBlocked || g.IsTrialReadOnly(now) == hardBlocked {
				t.Fatal("grant changed billing/access rules")
			}
		}
	}
}

func TestLaunchAccessOwnWhatsApp(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	now := time.Now()
	phone := "+525555555555"
	g := &Gym{SubscriptionPlan: PlanStandardMonthly, AllFeaturesAccess: true, WhatsAppBusinessPhone: &phone, WhatsAppConnectedAt: &now}
	if !g.UsesOwnWhatsAppNumber() {
		t.Fatal("launch gym should use its connected number")
	}
	g.WhatsAppConnectedAt = nil
	if g.UsesOwnWhatsAppNumber() {
		t.Fatal("grant must not fabricate a WhatsApp connection")
	}
}
