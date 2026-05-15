package controllers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	chkRepo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/accesswebhook"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// CheckinController bundles the cross-platform checkin endpoints — manual,
// PIN, override, plus the read-only widgets the reception screen polls
// (count today, methods, recent). The fingerprint endpoint + kiosk endpoints
// are wired by a separate sidecar-only controller
// (kiosk_controller.go, build tag sidecar).
type CheckinController struct {
	Manual               *chkApp.CheckinManual
	Pin                  *chkApp.CheckinByPin
	Override             *chkApp.OverrideCheckin
	Repo                 chkRepo.CheckinRepository
	UoW                  sharedDomain.UnitOfWork
	Tokens               auth.TokenService
	FingerprintAvailable func() bool
	// Webhook is invoked fire-and-forget after a successful "allowed"
	// checkin. nil → defaults to a no-op dispatcher.
	Webhook accesswebhook.Dispatcher
}

func NewCheckinController(
	manual *chkApp.CheckinManual,
	pin *chkApp.CheckinByPin,
	override *chkApp.OverrideCheckin,
	repo chkRepo.CheckinRepository,
	uow sharedDomain.UnitOfWork,
	fingerprint func() bool,
	tokens auth.TokenService,
) *CheckinController {
	if fingerprint == nil {
		fingerprint = func() bool { return false }
	}
	return &CheckinController{
		Manual: manual, Pin: pin, Override: override,
		Repo: repo, UoW: uow, FingerprintAvailable: fingerprint, Tokens: tokens,
		Webhook: accesswebhook.Noop(),
	}
}

// WithWebhook is a builder-style helper used at process bootstrap to swap
// the no-op dispatcher for an HTTP one (sidecar only). Returns the
// receiver so wiring code reads `NewCheckinController(...).WithWebhook(d)`.
func (c *CheckinController) WithWebhook(d accesswebhook.Dispatcher) *CheckinController {
	if d != nil {
		c.Webhook = d
	}
	return c
}

// dispatchAccessWebhook is invoked from each handler after a successful
// checkin. It interprets the AccessStatus on the view and fires the webhook
// when the result is one of the "allowed_*" states.
func (c *CheckinController) dispatchAccessWebhook(ctx *gin.Context, view *chkApp.CheckinView) {
	if c.Webhook == nil || view == nil {
		return
	}
	if !accesswebhook.ResultIsAllowed(string(view.AccessStatus)) {
		return
	}
	gymID, _ := middleware.GetGymID(ctx)
	c.Webhook.OnAccessAllowed(ctx.Request.Context(), accesswebhook.Event{
		Schema:     "cuadra.access.allowed/v1",
		GymID:      gymID,
		MemberID:   view.MemberID,
		MemberName: view.MemberName,
		Method:     view.Method,
		Result:     string(view.AccessStatus),
		OccurredAt: view.Timestamp,
	})
}

func (c *CheckinController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(c.Tokens))
	{
		api.POST("/checkins/manual", c.handleManual)
		api.POST("/checkins/pin", c.handlePin)
		api.POST("/checkins/override", c.handleOverride)
		api.GET("/checkins", c.handleListRecent)
		api.GET("/checkins/methods", c.handleMethods)
		api.GET("/checkins/count-today", c.handleCountToday)
		// Pestaña "Asistencia" del detalle de socio. Vive bajo /members/:id
		// para que el FE no tenga que armar query params raros — un GET y
		// listo.
		api.GET("/members/:id/checkins", c.handleListByMember)
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

// checkinEventWire matches the FE CheckinEvent shape (cuadra-desktop
// hooks/useCheckin.ts). Differences vs the application-layer CheckinView:
//   - id/timestamp/days field names are renamed to FE conventions.
//   - access_status (use-case-internal) is dropped — FE only consumes result.
type checkinEventWire struct {
	ID              uuid.UUID `json:"id"`
	MemberID        *string   `json:"member_id"`
	MemberName      *string   `json:"member_name"`
	Method          string    `json:"method"`
	Result          string    `json:"result"`
	ManualOverride  bool      `json:"manual_override"`
	OverrideReason  *string   `json:"override_reason,omitempty"`
	OperatorName    *string   `json:"operator_name,omitempty"`
	ExpiryDate      *string   `json:"expiry_date"`
	DaysUntilExpiry *int      `json:"days_until_expiry"`
	CreatedAt       time.Time `json:"created_at"`
}

func toCheckinResp(v *chkApp.CheckinView) checkinEventWire {
	r := checkinEventWire{
		ID:             v.CheckinID,
		Method:         v.Method,
		Result:         v.Result,
		ManualOverride: v.Override,
		CreatedAt:      v.Timestamp,
	}
	if v.MemberID != uuid.Nil {
		s := v.MemberID.String()
		r.MemberID = &s
	}
	if v.MemberName != "" {
		s := v.MemberName
		r.MemberName = &s
	}
	if v.ExpiryDate != nil {
		s := v.ExpiryDate.Format("2006-01-02")
		r.ExpiryDate = &s
	}
	if v.DaysToExpiry != nil {
		d := *v.DaysToExpiry
		r.DaysUntilExpiry = &d
	}
	return r
}

func recentToWire(r chkRepo.RecentCheckinRow) checkinEventWire {
	w := checkinEventWire{
		ID:             r.ID,
		MemberName:     r.MemberName,
		Method:         r.Method,
		Result:         r.Result,
		ManualOverride: r.ManualOverride,
		OverrideReason: r.OverrideReason,
		OperatorName:   r.OperatorName,
		CreatedAt:      r.CheckinAt,
	}
	if r.MemberID != nil {
		s := r.MemberID.String()
		w.MemberID = &s
	}
	if r.ExpiryDate != nil {
		s := r.ExpiryDate.Format("2006-01-02")
		w.ExpiryDate = &s
		// Days until expiry, day-granularity. Negative when the membership
		// already expired but the row still surfaced (recent list isn't
		// filtered by status).
		today := time.Now().UTC()
		days := int(time.Date(r.ExpiryDate.Year(), r.ExpiryDate.Month(), r.ExpiryDate.Day(), 0, 0, 0, 0, time.UTC).
			Sub(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24)
		w.DaysUntilExpiry = &days
	}
	return w
}

type checkinMethodsResp struct {
	FingerprintAvailable bool `json:"fingerprint_available"`
	PinAvailable         bool `json:"pin_available"`
	ManualAvailable      bool `json:"manual_available"`
}

type countTodayResp struct {
	CountToday int `json:"count_today"`
}

type recentCheckinsResp struct {
	Items []checkinEventWire `json:"items"`
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
	c.dispatchAccessWebhook(ctx, out)
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
	c.dispatchAccessWebhook(ctx, out)
	utils.JsonResponse(ctx, http.StatusCreated, toCheckinResp(out))
}

func (c *CheckinController) handleListRecent(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	limit := 20
	if v := ctx.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	// Drill-down support: when both `from` and `to` (YYYY-MM-DD) are
	// present, scope the query to that window instead of the default
	// "latest N recent". Used by reports drill-down dialogs.
	var fromT, toT time.Time
	if v := ctx.Query("from"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			fromT = t
		}
	}
	if v := ctx.Query("to"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			toT = t
		}
	}
	if c.Repo == nil || c.UoW == nil {
		utils.JsonResponse(ctx, http.StatusOK, recentCheckinsResp{Items: []checkinEventWire{}})
		return
	}
	tx, err := c.UoW.Query(ctx.Request.Context())
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusInternalServerError, err)
		return
	}
	var rows []chkRepo.RecentCheckinRow
	if !fromT.IsZero() && !toT.IsZero() {
		rows, err = c.Repo.ListByGymBetween(tx, gymID, fromT, toT, limit)
	} else {
		rows, err = c.Repo.ListRecentByGym(tx, gymID, limit)
	}
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusInternalServerError, err)
		return
	}
	items := make([]checkinEventWire, 0, len(rows))
	for _, r := range rows {
		items = append(items, recentToWire(r))
	}
	utils.JsonResponse(ctx, http.StatusOK, recentCheckinsResp{Items: items})
}

func (c *CheckinController) handleMethods(ctx *gin.Context) {
	utils.JsonResponse(ctx, http.StatusOK, checkinMethodsResp{
		FingerprintAvailable: c.FingerprintAvailable(),
		PinAvailable:         true,
		ManualAvailable:      true,
	})
}

func (c *CheckinController) handleCountToday(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	if c.Repo == nil || c.UoW == nil {
		utils.JsonResponse(ctx, http.StatusOK, countTodayResp{CountToday: 0})
		return
	}
	tx, err := c.UoW.Query(ctx.Request.Context())
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusInternalServerError, err)
		return
	}
	n, err := c.Repo.CountTodayByGym(tx, gymID, time.Now().UTC())
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusInternalServerError, err)
		return
	}
	utils.JsonResponse(ctx, http.StatusOK, countTodayResp{CountToday: n})
}

func (c *CheckinController) handleListByMember(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	memberID, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, errBadID)
		return
	}
	limit := 50
	if v := ctx.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if c.Repo == nil || c.UoW == nil {
		utils.JsonResponse(ctx, http.StatusOK, recentCheckinsResp{Items: []checkinEventWire{}})
		return
	}
	tx, err := c.UoW.Query(ctx.Request.Context())
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusInternalServerError, err)
		return
	}
	rows, err := c.Repo.ListByMemberDetailed(tx, gymID, memberID, limit)
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusInternalServerError, err)
		return
	}
	items := make([]checkinEventWire, 0, len(rows))
	for _, r := range rows {
		items = append(items, recentToWire(r))
	}
	utils.JsonResponse(ctx, http.StatusOK, recentCheckinsResp{Items: items})
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
