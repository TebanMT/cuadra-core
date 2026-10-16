package gym

import (
	"testing"
	"time"
)

func TestLaunchAccessDoesNotChangeBillingOrExpiration(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	features := []Feature{FeatureRecurringExpenses, FeatureAnalytics, FeatureReportExports, FeatureProductMargins, FeaturePromotionsROI, FeatureAuditLog, FeatureBranding, FeatureFiscalDetails, FeatureAccessHardware, FeatureOwnWhatsApp, FeatureBroadcasts, FeatureExtraOperators}
	for _, plan := range []string{PlanTrial, PlanStandardMonthly, PlanStandardAnnual, PlanPlusMonthly, PlanPlusAnnual} {
		for _, grant := range []bool{false, true} {
			g := &Gym{SubscriptionPlan: plan, AllFeaturesAccess: grant, SubscriptionStatus: StatusCancelled}
			for _, f := range features {
				if got := g.CanAccessFeature(f); got != (grant || IsPlusPlan(plan)) {
					t.Errorf("%s grant=%v feature=%s: %v", plan, grant, f, got)
				}
			}
			if g.CanAccessFeature("unknown_future_feature") {
				t.Fatal("unknown feature must fail closed")
			}
			if g.SubscriptionPlan != plan || g.HasActiveAccess(time.Now()) || !g.IsAccessHardBlocked(time.Now()) {
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
