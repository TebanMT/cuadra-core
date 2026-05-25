// Package controllers expone el BC promotions sobre HTTP. Owner-only en
// mutaciones; operador autenticado puede listar y buscar por código.
package controllers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	promoApp "github.com/cuadra/cuadra-core/src/modules/promotions/app"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var (
	errBadID   = errors.New("id inválido")
	errBadAuth = errors.New("autenticación requerida")
)

// PromotionController bundles los use cases del BC.
type PromotionController struct {
	Create         *promoApp.CreatePromotion
	Update         *promoApp.UpdatePromotion
	Deactivate     *promoApp.DeactivatePromotion
	Reactivate     *promoApp.ReactivatePromotion
	List           *promoApp.ListPromotions
	GetByCode      *promoApp.GetPromotionByCode
	AppliedSummary *promoApp.ListAppliedByMonth
	Tokens         auth.TokenService
}

func NewPromotionController(
	create *promoApp.CreatePromotion,
	update *promoApp.UpdatePromotion,
	deactivate *promoApp.DeactivatePromotion,
	reactivate *promoApp.ReactivatePromotion,
	list *promoApp.ListPromotions,
	getByCode *promoApp.GetPromotionByCode,
	appliedSummary *promoApp.ListAppliedByMonth,
	tokens auth.TokenService,
) *PromotionController {
	return &PromotionController{
		Create: create, Update: update, Deactivate: deactivate, Reactivate: reactivate,
		List: list, GetByCode: getByCode, AppliedSummary: appliedSummary, Tokens: tokens,
	}
}

func (ctrl *PromotionController) RegisterRoutes(r *gin.Engine) {
	g := r.Group("/api/v1/promotions")
	g.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		g.GET("", ctrl.handleList)
		g.GET("/by-code/:code", ctrl.handleGetByCode)
		g.POST("", middleware.RequireOwner(), ctrl.handleCreate)
		g.PATCH("/:id", middleware.RequireOwner(), ctrl.handleUpdate)
		g.DELETE("/:id", middleware.RequireOwner(), ctrl.handleDeactivate)
		g.POST("/:id/reactivate", middleware.RequireOwner(), ctrl.handleReactivate)
		g.GET("/applied/summary", middleware.RequireOwner(), ctrl.handleAppliedSummary)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type promotionReq struct {
	Name             string   `json:"name" validate:"required,min=3,max=100"`
	Description      *string  `json:"description,omitempty"`
	Kind             string   `json:"kind" validate:"required,oneof=percent fixed_amount free_enrollment extra_days companion_memberships"`
	Value            *float64 `json:"value,omitempty"`
	CompanionCount   *int     `json:"companion_count,omitempty"`
	AppliesTo        string   `json:"applies_to" validate:"required,oneof=membership sale any"`
	Code             *string  `json:"code,omitempty"`
	ValidFrom        *string  `json:"valid_from,omitempty"`
	ValidUntil       *string  `json:"valid_until,omitempty"`
	MaxUsesTotal     *int     `json:"max_uses_total,omitempty"`
	MaxUsesPerMember *int     `json:"max_uses_per_member,omitempty"`
}

type promotionResp struct {
	ID               uuid.UUID `json:"id"`
	Name             string    `json:"name"`
	Description      *string   `json:"description,omitempty"`
	Kind             string    `json:"kind"`
	Value            *float64  `json:"value,omitempty"`
	BuyN             int       `json:"buy_n"`
	CompanionCount   *int      `json:"companion_count,omitempty"`
	AppliesTo        string    `json:"applies_to"`
	Code             *string   `json:"code,omitempty"`
	ValidFrom        *string   `json:"valid_from,omitempty"`
	ValidUntil       *string   `json:"valid_until,omitempty"`
	MaxUsesTotal     *int      `json:"max_uses_total,omitempty"`
	MaxUsesPerMember *int      `json:"max_uses_per_member,omitempty"`
	Active           bool      `json:"active"`
}

type appliedSummaryResp struct {
	PromotionID    uuid.UUID `json:"promotion_id"`
	PromotionName  string    `json:"promotion_name"`
	Kind           string    `json:"kind"`
	UseCount       int       `json:"use_count"`
	TotalDiscount  float64   `json:"total_discount"`
	MembersReached int       `json:"members_reached"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func bindJSON[T any](c *gin.Context, dst *T) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return false
	}
	if err := utils.ValidateRequest(*dst); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return false
	}
	return true
}

func parseUUIDParam(c *gin.Context, name string) (uuid.UUID, bool) {
	raw := c.Param(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, errBadID)
		return uuid.Nil, false
	}
	return id, true
}

func parseDate(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", *s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func toResp(p *promoDomain.Promotion) promotionResp {
	r := promotionResp{
		ID: p.ID, Name: p.Name, Description: p.Description,
		Kind: p.Kind, Value: p.Value, BuyN: p.BuyN,
		CompanionCount: p.CompanionCount, AppliesTo: p.AppliesTo,
		Code: p.Code, MaxUsesTotal: p.MaxUsesTotal,
		MaxUsesPerMember: p.MaxUsesPerMember, Active: p.Active,
	}
	if p.ValidFrom != nil {
		s := p.ValidFrom.Format("2006-01-02")
		r.ValidFrom = &s
	}
	if p.ValidUntil != nil {
		s := p.ValidUntil.Format("2006-01-02")
		r.ValidUntil = &s
	}
	return r
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (ctrl *PromotionController) handleCreate(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	var req promotionReq
	if !bindJSON(c, &req) {
		return
	}
	from, err := parseDate(req.ValidFrom)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	until, err := parseDate(req.ValidUntil)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	out, err := ctrl.Create.Execute(c.Request.Context(), promoApp.CreatePromotionInput{
		GymID: gymID, ActorUserID: userID,
		Name: req.Name, Description: req.Description,
		Kind: req.Kind, Value: req.Value, CompanionCount: req.CompanionCount,
		AppliesTo: req.AppliesTo, Code: req.Code,
		ValidFrom: from, ValidUntil: until,
		MaxUsesTotal: req.MaxUsesTotal, MaxUsesPerMember: req.MaxUsesPerMember,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusCreated, toResp(out))
}

func (ctrl *PromotionController) handleUpdate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req promotionReq
	if !bindJSON(c, &req) {
		return
	}
	from, err := parseDate(req.ValidFrom)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	until, err := parseDate(req.ValidUntil)
	if err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	out, err := ctrl.Update.Execute(c.Request.Context(), promoApp.UpdatePromotionInput{
		GymID: gymID, ActorUserID: userID, PromotionID: id,
		Name: req.Name, Description: req.Description,
		Kind: req.Kind, Value: req.Value, CompanionCount: req.CompanionCount,
		AppliesTo: req.AppliesTo, Code: req.Code,
		ValidFrom: from, ValidUntil: until,
		MaxUsesTotal: req.MaxUsesTotal, MaxUsesPerMember: req.MaxUsesPerMember,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toResp(out))
}

func (ctrl *PromotionController) handleDeactivate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.Deactivate.Execute(c.Request.Context(), promoApp.DeactivatePromotionInput{
		GymID: gymID, ActorUserID: userID, PromotionID: id,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toResp(out))
}

func (ctrl *PromotionController) handleReactivate(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	out, err := ctrl.Reactivate.Execute(c.Request.Context(), promoApp.DeactivatePromotionInput{
		GymID: gymID, ActorUserID: userID, PromotionID: id,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toResp(out))
}

func (ctrl *PromotionController) handleList(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	includeInactive := c.Query("include_inactive") == "true"
	appliesTo := c.Query("applies_to")
	var today *time.Time
	if c.Query("currently_valid") == "true" {
		t := time.Now().UTC()
		today = &t
	}
	out, err := ctrl.List.Execute(c.Request.Context(), promoApp.ListPromotionsInput{
		GymID: gymID, IncludeInactive: includeInactive,
		AppliesTo: appliesTo, CurrentlyValid: today,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	resp := make([]promotionResp, 0, len(out))
	for _, p := range out {
		resp = append(resp, toResp(p))
	}
	utils.JsonResponse(c, http.StatusOK, resp)
}

func (ctrl *PromotionController) handleGetByCode(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	code := strings.TrimSpace(c.Param("code"))
	t := time.Now().UTC()
	out, err := ctrl.GetByCode.Execute(c.Request.Context(), promoApp.GetPromotionByCodeInput{
		GymID: gymID, Code: code, Today: &t,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, toResp(out))
}

func (ctrl *PromotionController) handleAppliedSummary(c *gin.Context) {
	gymID, _ := middleware.GetGymID(c)
	month := c.Query("month")
	out, err := ctrl.AppliedSummary.Execute(c.Request.Context(), promoApp.ListAppliedByMonthInput{
		GymID: gymID, Month: month,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	resp := make([]appliedSummaryResp, 0, len(out))
	for _, s := range out {
		resp = append(resp, appliedSummaryResp{
			PromotionID: s.PromotionID, PromotionName: s.PromotionName,
			Kind: s.Kind, UseCount: s.UseCount,
			TotalDiscount: s.TotalDiscount, MembersReached: s.MembersReached,
		})
	}
	utils.JsonResponse(c, http.StatusOK, resp)
}
