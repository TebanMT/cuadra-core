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

// RequirePlusPlan corre DESPUÉS de AuthMiddleware (lee gym_id de las claims
// stash-eadas) y aborta con 402 si el gym no está en Plus. Trial y Standard
// NO pasan mientras Plus no se libere (ver gymDomain.CanAccessPlusFeatures
// para el WHY y cuándo revertir).
//
// Fail-open: si el repo/UoW son nil (tests que no montan dominio) o el
// lookup falla, dejamos pasar — preferimos un riesgo de "operó con Plus
// indebidamente" a "bug del lookup bloquea al gym entero". El audit del
// uso es la red de seguridad post-hoc.
func RequirePlusPlan(gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork) gin.HandlerFunc {
	if gyms == nil || uow == nil {
		return func(c *gin.Context) { c.Next() }
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
		if err != nil {
			c.Next()
			return
		}
		g, err := gyms.GetByID(tx, gymID)
		if err != nil || g == nil {
			c.Next()
			return
		}
		if gymDomain.CanAccessPlusFeatures(g.SubscriptionPlan) {
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
		return true
	}
	gymID, ok := GetGymID(c)
	if !ok {
		return true
	}
	tx, err := uow.Query(c.Request.Context())
	if err != nil {
		return true
	}
	g, err := gyms.GetByID(tx, gymID)
	if err != nil || g == nil {
		return true
	}
	if gymDomain.CanAccessPlusFeatures(g.SubscriptionPlan) {
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
