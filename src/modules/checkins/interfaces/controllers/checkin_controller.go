package controllers

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// CheckinController bundles the cross-platform checkin endpoints — manual,
// PIN, override. The fingerprint endpoint + kiosk endpoints are wired by a
// separate sidecar-only controller (kiosk_controller.go, build tag sidecar).
type CheckinController struct {
	Manual   *chkApp.CheckinManual
	Pin      *chkApp.CheckinByPin
	Override *chkApp.OverrideCheckin
	Tokens   auth.TokenService
}

func NewCheckinController(
	manual *chkApp.CheckinManual,
	pin *chkApp.CheckinByPin,
	override *chkApp.OverrideCheckin,
	tokens auth.TokenService,
) *CheckinController {
	return &CheckinController{Manual: manual, Pin: pin, Override: override, Tokens: tokens}
}

func (c *CheckinController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(c.Tokens))
	{
		api.POST("/checkins/manual", c.handleManual)
		api.POST("/checkins/pin", c.handlePin)
		api.POST("/checkins/override", c.handleOverride)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type manualReq struct {
	MemberID string `json:"member_id" binding:"required"`
}

type pinReq struct {
	Pin string `json:"pin" binding:"required"`
}

type overrideReq struct {
	MemberID       string `json:"member_id" binding:"required"`
	OriginalMethod string `json:"original_method,omitempty"`
	Reason         string `json:"reason" binding:"required"`
}

type checkinResp struct {
	CheckinID    uuid.UUID `json:"checkin_id"`
	MemberID     uuid.UUID `json:"member_id"`
	MemberName   string    `json:"member_name"`
	Method       string    `json:"method"`
	Result       string    `json:"result"`
	AccessStatus string    `json:"access_status"`
	ExpiryDate   *string   `json:"expiry_date,omitempty"`
	DaysToExpiry *int      `json:"days_to_expiry,omitempty"`
	Override     bool      `json:"override"`
	Timestamp    time.Time `json:"timestamp"`
}

func toCheckinResp(v *chkApp.CheckinView) checkinResp {
	r := checkinResp{
		CheckinID:    v.CheckinID,
		MemberID:     v.MemberID,
		MemberName:   v.MemberName,
		Method:       v.Method,
		Result:       v.Result,
		AccessStatus: string(v.AccessStatus),
		Override:     v.Override,
		Timestamp:    v.Timestamp,
	}
	if v.ExpiryDate != nil {
		s := v.ExpiryDate.Format("2006-01-02")
		r.ExpiryDate = &s
	}
	if v.DaysToExpiry != nil {
		d := *v.DaysToExpiry
		r.DaysToExpiry = &d
	}
	return r
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

var errBadID = errors.New("id inválido")

func (c *CheckinController) handleManual(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	operator, _ := middleware.GetUserID(ctx)
	var req manualReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, err)
		return
	}
	memberID, err := uuid.Parse(req.MemberID)
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, errBadID)
		return
	}
	out, err := c.Manual.Execute(ctx.Request.Context(), chkApp.CheckinManualInput{
		GymID: gymID, OperatorID: operator, MemberID: memberID,
	})
	if err != nil {
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(ctx, http.StatusCreated, toCheckinResp(out))
}

func (c *CheckinController) handlePin(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	var req pinReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, err)
		return
	}
	out, err := c.Pin.Execute(ctx.Request.Context(), chkApp.CheckinByPinInput{
		GymID: gymID, Pin: req.Pin,
	})
	if err != nil {
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(ctx, http.StatusCreated, toCheckinResp(out))
}

func (c *CheckinController) handleOverride(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	operator, _ := middleware.GetUserID(ctx)
	var req overrideReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, err)
		return
	}
	memberID, err := uuid.Parse(req.MemberID)
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, errBadID)
		return
	}
	out, err := c.Override.Execute(ctx.Request.Context(), chkApp.OverrideCheckinInput{
		GymID: gymID, OperatorID: operator, MemberID: memberID,
		OriginalMethod: req.OriginalMethod, Reason: req.Reason,
	})
	if err != nil {
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(ctx, http.StatusCreated, toCheckinResp(out))
}
