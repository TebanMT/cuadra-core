// Package controllers — wires HTTP for the members BC (UC-011..UC-017, UC-032).
//
// Owner-only endpoints: membership_types CRUD + lock-expiry. Owner+Operator:
// member CRUD, list, detail, status-toggle, PIN assign.
package controllers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// MembershipTypeController exposes UC-011 endpoints.
type MembershipTypeController struct {
	Create     *memApp.CreateMembershipType
	Update     *memApp.UpdateMembershipType
	Deactivate *memApp.DeactivateMembershipType
	List       *memApp.ListMembershipTypes
	Tokens     auth.TokenService
}

func NewMembershipTypeController(
	create *memApp.CreateMembershipType,
	update *memApp.UpdateMembershipType,
	deactivate *memApp.DeactivateMembershipType,
	list *memApp.ListMembershipTypes,
	tokens auth.TokenService,
) *MembershipTypeController {
	return &MembershipTypeController{
		Create: create, Update: update, Deactivate: deactivate, List: list, Tokens: tokens,
	}
}

func (ctrl *MembershipTypeController) RegisterRoutes(r *gin.Engine) {
	g := r.Group("/api/v1/membership-types")
	g.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		g.POST("", middleware.RequireOwner(), ctrl.handleCreate)
		g.GET("", ctrl.handleList)
		g.PATCH("/:id", middleware.RequireOwner(), ctrl.handleUpdate)
		g.DELETE("/:id", middleware.RequireOwner(), ctrl.handleDeactivate)
	}
}

type membershipTypeReq struct {
	Name                 string  `json:"name" validate:"required,min=3,max=100"`
	Price                float64 `json:"price" validate:"required,gt=0"`
	DurationDays         int     `json:"duration_days" validate:"required,gt=0"`
	EnrollmentFee        float64 `json:"enrollment_fee"`
	MaintenanceFee       float64 `json:"maintenance_fee"`
	MaintenanceFrequency string  `json:"maintenance_frequency"`
}

type membershipTypeResp struct {
	ID                   uuid.UUID `json:"id"`
	Name                 string    `json:"name"`
	Price                float64   `json:"price"`
	DurationDays         int       `json:"duration_days"`
	EnrollmentFee        float64   `json:"enrollment_fee"`
	MaintenanceFee       float64   `json:"maintenance_fee"`
	MaintenanceFrequency *string   `json:"maintenance_frequency"`
	Active               bool      `json:"active"`
}

var errBadAuth = errors.New("token de autenticación faltante o inválido")

func (ctrl *MembershipTypeController) handleCreate(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req membershipTypeReq
	if !bindJSON(c, &req) {
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
	utils.JsonResponse(c, http.StatusCreated, membershipTypeResp{
		ID: out.ID, Name: out.Name, Price: out.Price, DurationDays: out.DurationDays,
		EnrollmentFee: out.EnrollmentFee, MaintenanceFee: out.MaintenanceFee,
		MaintenanceFrequency: out.MaintenanceFrequency, Active: out.Active,
	})
}

func (ctrl *MembershipTypeController) handleUpdate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req membershipTypeReq
	if !bindJSON(c, &req) {
		return
	}
	out, err := ctrl.Update.Execute(c.Request.Context(), memApp.UpdateMembershipTypeInput{
		GymID: gymID, ActorUserID: userID, TypeID: id,
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
	utils.JsonResponse(c, http.StatusOK, membershipTypeResp{
		ID: out.ID, Name: out.Name, Price: out.Price, DurationDays: out.DurationDays,
		EnrollmentFee: out.EnrollmentFee, MaintenanceFee: out.MaintenanceFee,
		MaintenanceFrequency: out.MaintenanceFrequency, Active: out.Active,
	})
}

func (ctrl *MembershipTypeController) handleDeactivate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.Deactivate.Execute(c.Request.Context(), memApp.DeactivateMembershipTypeInput{
		GymID: gymID, ActorUserID: userID, TypeID: id,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, membershipTypeResp{
		ID: out.ID, Name: out.Name, Price: out.Price, DurationDays: out.DurationDays,
		EnrollmentFee: out.EnrollmentFee, MaintenanceFee: out.MaintenanceFee,
		MaintenanceFrequency: out.MaintenanceFrequency, Active: out.Active,
	})
}

func (ctrl *MembershipTypeController) handleList(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	includeInactive := c.Query("include_inactive") == "true"
	out, err := ctrl.List.Execute(c.Request.Context(), memApp.ListMembershipTypesInput{
		GymID: gymID, IncludeInactive: includeInactive,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	resp := make([]membershipTypeResp, 0, len(out))
	for _, mt := range out {
		resp = append(resp, membershipTypeResp{
			ID: mt.ID, Name: mt.Name, Price: mt.Price, DurationDays: mt.DurationDays,
			EnrollmentFee: mt.EnrollmentFee, MaintenanceFee: mt.MaintenanceFee,
			MaintenanceFrequency: mt.MaintenanceFrequency, Active: mt.Active,
		})
	}
	utils.JsonResponse(c, http.StatusOK, resp)
}
