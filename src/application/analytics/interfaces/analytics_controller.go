// Package interfaces expone el analytics Plus sobre HTTP. SOLO lo registra
// cmd/server (la pestaña Análisis es del dashboard cloud); el sidecar no
// monta estas rutas y por eso no existe reader SQLite.
package interfaces

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	analyticsApp "github.com/cuadra/cuadra-core/src/application/analytics"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var errBadAuth = errors.New("autenticación requerida")

type AnalyticsController struct {
	Overview *analyticsApp.Overview
	Tokens   auth.TokenService
	// PlanGate — middleware.RequirePlusPlan del main. Analytics es EL
	// contenido que vende Plus: sin gate no hay tier.
	PlanGate gin.HandlerFunc
}

func NewAnalyticsController(overview *analyticsApp.Overview, tokens auth.TokenService) *AnalyticsController {
	return &AnalyticsController{Overview: overview, Tokens: tokens}
}

func (ctrl *AnalyticsController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	// Owner-only (defensa en profundidad — el cloud ya sólo loguea dueños)
	// + PlanGate: Standard recibe 402 plan_required y el FE muestra el
	// candado/upsell.
	grp := api.Group("", middleware.RequireOwner())
	if ctrl.PlanGate != nil {
		grp.Use(ctrl.PlanGate)
	}
	grp.GET("/analytics", ctrl.handleOverview)
	grp.GET("/analytics/promotions-roi", ctrl.handlePromotionsROI)
}

func (ctrl *AnalyticsController) handleOverview(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	out, err := ctrl.Overview.Execute(c.Request.Context(), analyticsApp.OverviewInput{GymID: gymID})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, out)
}

func (ctrl *AnalyticsController) handlePromotionsROI(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	out, err := ctrl.Overview.PromotionsROIReport(c.Request.Context(), analyticsApp.PromotionsROIQuery{GymID: gymID})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, out)
}
