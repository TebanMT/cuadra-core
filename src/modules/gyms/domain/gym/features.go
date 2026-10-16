package gym

// Feature is a stable entitlement key. Every new gated feature must declare
// its minimum tier here; the launch grant includes future registered features.
// Access to an operation still requires its role and subscription checks.
type Feature string
type FeatureTier string

const (
	TierStandard             FeatureTier = "standard"
	TierPlus                 FeatureTier = "plus"
	FeatureDailyOperations   Feature     = "daily_operations"
	FeatureRecurringExpenses Feature     = "recurring_expenses"
	FeatureAnalytics         Feature     = "analytics"
	FeatureReportExports     Feature     = "report_exports"
	FeatureProductMargins    Feature     = "product_margins"
	FeaturePromotionsROI     Feature     = "promotions_roi"
	FeatureAuditLog          Feature     = "audit_log"
	FeatureBranding          Feature     = "branding"
	FeatureFiscalDetails     Feature     = "fiscal_details"
	FeatureAccessHardware    Feature     = "access_hardware"
	FeatureOwnWhatsApp       Feature     = "own_whatsapp"
	FeatureBroadcasts        Feature     = "broadcasts"
	FeatureHeldMessages      Feature     = "held_messages"
	FeatureExtraOperators    Feature     = "extra_operators"
)

// MinimumTier is deliberately explicit and fails closed for unknown keys.
func MinimumTier(feature Feature) (FeatureTier, bool) {
	switch feature {
	case FeatureDailyOperations:
		return TierStandard, true
	case FeatureRecurringExpenses, FeatureAnalytics, FeatureReportExports,
		FeatureProductMargins, FeaturePromotionsROI, FeatureAuditLog, FeatureBranding,
		FeatureFiscalDetails, FeatureAccessHardware, FeatureOwnWhatsApp,
		FeatureBroadcasts, FeatureExtraOperators, FeatureHeldMessages:
		return TierPlus, true
	default:
		return "", false
	}
}

func (g *Gym) CanAccessFeature(feature Feature) bool {
	if g == nil {
		return false
	}
	tier, known := MinimumTier(feature)
	if !known {
		return false
	}
	if tier == TierStandard {
		return g.SubscriptionPlan == PlanTrial || IsPaidPlan(g.SubscriptionPlan)
	}
	return CanAccessPlusFeatures(g.SubscriptionPlan, g.AllFeaturesAccess)
}
