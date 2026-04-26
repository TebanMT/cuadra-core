// HTTP-side wiring for the reports application layer (UC-033..UC-036) plus
// the persecución mutations (UC-035 mark contacted / mark lost). These all
// live in one controller because they share the same audience (owner) and
// the same auth surface; splitting would only add boilerplate.
package interfaces

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

var (
	errBadID   = errors.New("id inválido")
	errBadAuth = errors.New("autenticación requerida")
)

// ReportsController bundles UC-033..UC-036 plus UC-035 persecución mutations.
type ReportsController struct {
	Dashboard         *reportsApp.Dashboard
	AttentionRequired *reportsApp.AttentionRequired
	Export            *reportsApp.ExportReport
	MarkContacted     *memApp.MarkContacted
	MarkLost          *memApp.MarkLost
	Tokens            auth.TokenService
}

func NewReportsController(
	dashboard *reportsApp.Dashboard,
	attention *reportsApp.AttentionRequired,
	export *reportsApp.ExportReport,
	markContacted *memApp.MarkContacted,
	markLost *memApp.MarkLost,
	tokens auth.TokenService,
) *ReportsController {
	return &ReportsController{
		Dashboard: dashboard, AttentionRequired: attention, Export: export,
		MarkContacted: markContacted, MarkLost: markLost, Tokens: tokens,
	}
}

func (ctrl *ReportsController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(ctrl.Tokens))
	{
		api.GET("/dashboard", ctrl.handleDashboard)
		api.GET("/attention-required", ctrl.handleAttentionRequired)
		api.GET("/reports/:type/export", ctrl.handleExport)
		api.POST("/members/:id/contact-attempts", ctrl.handleMarkContacted)
		api.POST("/members/:id/mark-lost", ctrl.handleMarkLost)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type contactAttemptReq struct {
	Channel *string `json:"channel,omitempty"`
	Note    *string `json:"note,omitempty"`
}

type markLostReq struct {
	Reason string `json:"reason,omitempty"`
}

type contactAttemptResp struct {
	ContactAttemptID     uuid.UUID `json:"contact_attempt_id"`
	MemberID             uuid.UUID `json:"member_id"`
	LastContactAttemptAt time.Time `json:"last_contact_attempt_at"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (ctrl *ReportsController) handleDashboard(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	out, err := ctrl.Dashboard.Execute(c.Request.Context(), reportsApp.DashboardInput{GymID: gymID})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, out)
}

func (ctrl *ReportsController) handleAttentionRequired(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	out, err := ctrl.AttentionRequired.Execute(c.Request.Context(), reportsApp.AttentionRequiredInput{GymID: gymID})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(c, http.StatusOK, out)
}

func (ctrl *ReportsController) handleExport(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	reportType := c.Param("type")
	format := c.DefaultQuery("format", reportsApp.FormatPDF)

	in := reportsApp.ExportInput{GymID: gymID, Type: reportType, Format: format}
	if v := c.Query("from"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		in.From = &t
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			utils.ErrorResponse(c, http.StatusBadRequest, err)
			return
		}
		in.To = &t
	}
	out, err := ctrl.Export.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+out.Filename+`"`)
	c.Header("Content-Length", strconv.Itoa(len(out.Bytes)))
	c.Data(http.StatusOK, out.ContentType, out.Bytes)
}

func (ctrl *ReportsController) handleMarkContacted(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req contactAttemptReq
	_ = c.ShouldBindJSON(&req)
	out, err := ctrl.MarkContacted.Execute(c.Request.Context(), memApp.MarkContactedInput{
		GymID: gymID, ActorUserID: userID, MemberID: id,
		Channel: req.Channel, Note: req.Note,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	ctrl.Dashboard.InvalidateCache(gymID)
	utils.JsonResponse(c, http.StatusCreated, contactAttemptResp{
		ContactAttemptID:     out.ContactAttemptID,
		MemberID:             out.MemberID,
		LastContactAttemptAt: out.LastContactAttemptAt,
	})
}

func (ctrl *ReportsController) handleMarkLost(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		utils.ErrorResponse(c, http.StatusUnauthorized, errBadAuth)
		return
	}
	userID, _ := middleware.GetUserID(c)
	id, ok := parseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req markLostReq
	_ = c.ShouldBindJSON(&req)
	out, err := ctrl.MarkLost.Execute(c.Request.Context(), memApp.MarkLostInput{
		GymID: gymID, ActorUserID: userID, MemberID: id, Reason: req.Reason,
	})
	if err != nil {
		utils.ErrorResponse(c, utils.DomainErrorToHttpCode(err), err)
		return
	}
	ctrl.Dashboard.InvalidateCache(gymID)
	utils.JsonResponse(c, http.StatusOK, gin.H{
		"member_id": out.ID,
		"status":    out.Status,
	})
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
