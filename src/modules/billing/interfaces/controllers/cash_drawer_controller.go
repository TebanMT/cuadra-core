package controllers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	cashclose "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

type CashDrawerController struct {
	UseCases *billingApp.CashDrawers
	Tokens   auth.TokenService
}

func NewCashDrawerController(useCases *billingApp.CashDrawers, tokens auth.TokenService) *CashDrawerController {
	return &CashDrawerController{UseCases: useCases, Tokens: tokens}
}

func (ctrl *CashDrawerController) RegisterRoutes(router *gin.Engine) {
	group := router.Group("/api/v1/cash-drawers")
	group.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		group.GET("", ctrl.handleList)
		group.POST("", middleware.RequireOwner(), ctrl.handleCreate)
		group.PATCH("/:id", middleware.RequireOwner(), ctrl.handleUpdate)
		group.DELETE("/:id", middleware.RequireOwner(), ctrl.handleDeactivate)
	}
}

type createCashDrawerReq struct {
	Name           string `json:"name" validate:"required,min=1,max=60"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type updateCashDrawerReq struct {
	Name    *string `json:"name,omitempty" validate:"omitempty,min=1,max=60"`
	Active  *bool   `json:"active,omitempty"`
	Version int     `json:"version" validate:"required,min=1"`
}

type cashDrawerCatalogResp struct {
	ID      uuid.UUID `json:"id"`
	Code    string    `json:"code"`
	Name    string    `json:"name"`
	Active  bool      `json:"active"`
	IsMain  bool      `json:"is_main"`
	Version int       `json:"version"`
}

func (ctrl *CashDrawerController) handleList(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	includeInactive, _ := strconv.ParseBool(c.DefaultQuery("include_inactive", "false"))
	rows, err := ctrl.UseCases.List(c.Request.Context(), billingApp.ListCashDrawersInput{
		GymID: gymID, IncludeInactive: includeInactive,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	items := make([]cashDrawerCatalogResp, 0, len(rows))
	for _, row := range rows {
		items = append(items, toCashDrawerResp(row))
	}
	utils.JsonResponse(c, http.StatusOK, gin.H{"items": items})
}

func (ctrl *CashDrawerController) handleCreate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	var req createCashDrawerReq
	if !bindJSON(c, &req) {
		return
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		key = strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	}
	if key == "" || len(key) > 120 {
		utils.ErrorResponse(c, http.StatusBadRequest, cashclose.ErrDrawerIdempotencyRequired)
		return
	}
	row, err := ctrl.UseCases.Create(c.Request.Context(), billingApp.CreateCashDrawerInput{
		GymID: gymID, ActorUserID: actorID, ActorRole: role, Name: req.Name, IdempotencyKey: key,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, toCashDrawerResp(row))
}

func (ctrl *CashDrawerController) handleUpdate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	drawerID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req updateCashDrawerReq
	if !bindJSON(c, &req) {
		return
	}
	row, err := ctrl.UseCases.Update(c.Request.Context(), billingApp.UpdateCashDrawerInput{
		GymID: gymID, ActorUserID: actorID, ActorRole: role, DrawerID: drawerID,
		Name: req.Name, Active: req.Active, ExpectedVersion: req.Version,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toCashDrawerResp(row))
}

func (ctrl *CashDrawerController) handleDeactivate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	actorID, _ := middleware.GetUserID(c)
	role, _ := middleware.GetRole(c)
	drawerID, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	version, err := strconv.Atoi(c.Query("version"))
	if err != nil || version < 1 {
		utils.ErrorResponse(c, http.StatusBadRequest, cashclose.ErrDrawerVersionConflict)
		return
	}
	if _, err := ctrl.UseCases.Deactivate(c.Request.Context(), gymID, actorID, role, drawerID, version); err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Status(http.StatusNoContent)
}

func toCashDrawerResp(row *cashclose.CashDrawer) cashDrawerCatalogResp {
	return cashDrawerCatalogResp{ID: row.ID, Code: row.Code, Name: row.Name,
		Active: row.Active, IsMain: row.IsMain, Version: row.Version}
}
