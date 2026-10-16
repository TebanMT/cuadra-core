package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// PlanGateResponse es el body del 402 cuando una ruta exige Plus. El FE lo
// detecta para mostrar la pantalla / banner de upsell con CTA hacia
// /settings/subscription. "current_plan" viaja para que el copy del upsell
// hable de "Estás en Standard" vs el genérico "necesitas Plus".
type PlanGateResponse struct {
	Error        string `json:"error"`         // siempre "plan_required"
	RequiredPlan string `json:"required_plan"` // "plus"
	CurrentPlan  string `json:"current_plan"`
	Message      string `json:"message"`
}

// RequirePlusPlan retains the bundle gate for callers that expose several Plus
// features together. New feature-specific routes should use RequireFeature.
func RequirePlusPlan(gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork) gin.HandlerFunc {
	return RequireFeature(gyms, uow, gymDomain.FeatureAnalytics)
}

// RequireFeature uses the formal feature catalog, including launch grants.
func RequireFeature(gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork, feature gymDomain.Feature) gin.HandlerFunc {
	if gyms == nil || uow == nil {
		return func(c *gin.Context) { abortPlanCheckUnavailable(c) }
	}
	return func(c *gin.Context) {
		gymID, ok := GetGymID(c)
		if !ok {
			// Sin claims → la cadena de auth devolverá 401 en su debido
			// momento; no es nuestro lugar de decisión.
			c.Next()
			return
		}
		tx, err := uow.Query(c.Request.Context())
		defer sharedDomain.CloseQuery(tx)
		if err != nil {
			abortPlanCheckUnavailable(c)
			return
		}
		g, err := gyms.GetByID(tx, gymID)
		sharedDomain.CloseQuery(tx)
		if err != nil || g == nil {
			abortPlanCheckUnavailable(c)
			return
		}
		if g.CanAccessFeature(feature) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusPaymentRequired, PlanGateResponse{
			Error:        "plan_required",
			RequiredPlan: "plus",
			CurrentPlan:  g.SubscriptionPlan,
			Message:      "Esta acción requiere el plan Plus. Mejora tu suscripción para usarla.",
		})
	}
}

// EnforcePlusInline corre la misma decisión que RequirePlusPlan pero como
// llamada desde un handler — útil cuando un endpoint mezcla acciones
// Standard y Plus (p. ej. PATCH /gyms/me que toca branding[Plus] y
// city[Standard], o POST /broadcasts dentro del controller de
// notifications que también tiene rutas Standard). Retorna true si el
// caller debe seguir; false si ya abortó la response con 402.
func EnforcePlusInline(c *gin.Context, gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork) bool {
	if gyms == nil || uow == nil {
		abortPlanCheckUnavailable(c)
		return false
	}
	gymID, ok := GetGymID(c)
	if !ok {
		return true
	}
	tx, err := uow.Query(c.Request.Context())
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		abortPlanCheckUnavailable(c)
		return false
	}
	g, err := gyms.GetByID(tx, gymID)
	if err != nil || g == nil {
		abortPlanCheckUnavailable(c)
		return false
	}
	if gymDomain.CanAccessPlusFeatures(g.SubscriptionPlan, g.AllFeaturesAccess) {
		return true
	}
	c.AbortWithStatusJSON(http.StatusPaymentRequired, PlanGateResponse{
		Error:        "plan_required",
		RequiredPlan: "plus",
		CurrentPlan:  g.SubscriptionPlan,
		Message:      "Esta acción requiere el plan Plus. Mejora tu suscripción para usarla.",
	})
	return false
}

func abortPlanCheckUnavailable(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"error":   "plan_check_unavailable",
		"message": "No pudimos verificar tu plan en este momento. Intenta de nuevo.",
	})
}
