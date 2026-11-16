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
//
// Oferta vigente (decisión del fundador, 16 nov 2026): un solo plan Standard
// con todo lo que existe hoy, igual que la prueba, como promete la landing.
// Plus conserva sólo lo que no se vende todavía: conectar el número propio de
// WhatsApp y los envíos masivos sin tope. En Standard esos envíos salen por el
// número de Tinta con los topes de notifications/app/broadcast.go, porque el
// costo de cada mensaje lo absorbe Tinta.
func MinimumTier(feature Feature) (FeatureTier, bool) {
	switch feature {
	case FeatureDailyOperations, FeatureRecurringExpenses, FeatureAnalytics,
		FeatureReportExports, FeatureProductMargins, FeaturePromotionsROI,
		FeatureAuditLog, FeatureBranding, FeatureFiscalDetails,
		FeatureAccessHardware, FeatureHeldMessages, FeatureExtraOperators:
		return TierStandard, true
	case FeatureOwnWhatsApp, FeatureBroadcasts:
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
