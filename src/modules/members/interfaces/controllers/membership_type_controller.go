// Package controllers — wires HTTP for the members BC. In Sesión 1 we only
// expose POST /api/v1/membership-types because the wizard needs it.
package controllers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

type MembershipTypeController struct {
	Create *memApp.CreateMembershipType
	Tokens auth.TokenService
}

func NewMembershipTypeController(create *memApp.CreateMembershipType, tokens auth.TokenService) *MembershipTypeController {
	return &MembershipTypeController{Create: create, Tokens: tokens}
}

func (ctrl *MembershipTypeController) RegisterRoutes(r *gin.Engine) {
	g := r.Group("/api/v1/membership-types")
	g.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		g.POST("", ctrl.handleCreate)
	}
}

type createReq struct {
	Name                 string  `json:"name" validate:"required,min=1,max=100"`
	Price                float64 `json:"price" validate:"required,gt=0"`
	DurationDays         int     `json:"duration_days" validate:"required,gt=0"`
	EnrollmentFee        float64 `json:"enrollment_fee"`
	MaintenanceFee       float64 `json:"maintenance_fee"`
	MaintenanceFrequency string  `json:"maintenance_frequency"`
}

var errBadAuth = errors.New("token de autenticación faltante o inválido")

func (ctrl *MembershipTypeController) handleCreate(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req createReq
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	if err := utils.ValidateRequest(req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	out, err := ctrl.Create.Execute(c.Request.Context(), memApp.CreateMembershipTypeInput{
		GymID: gymID, ActorUserID: userID,
		Name:                 req.Name,
		Price:                req.Price,
		DurationDays:         req.DurationDays,
		EnrollmentFee:        req.EnrollmentFee,
		MaintenanceFee:       req.MaintenanceFee,
		MaintenanceFrequency: req.MaintenanceFrequency,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, gin.H{
		"id":            out.ID,
		"name":          out.Name,
		"price":         out.Price,
		"duration_days": out.DurationDays,
		"active":        out.Active,
	})
}
