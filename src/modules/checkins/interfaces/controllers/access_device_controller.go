package controllers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// AccessDeviceController exposes only the local sidecar hardware surface. The
// routes are owner-only because pulsing an output can become opening a door.
type AccessDeviceController struct {
	Test   *chkApp.TestAccessActuator
	Status *chkApp.GetAccessActuatorStatus
	Tokens auth.TokenService
}

func NewAccessDeviceController(
	test *chkApp.TestAccessActuator,
	status *chkApp.GetAccessActuatorStatus,
	tokens auth.TokenService,
) *AccessDeviceController {
	return &AccessDeviceController{Test: test, Status: status, Tokens: tokens}
}

func (c *AccessDeviceController) RegisterRoutes(r *gin.Engine) {
	g := r.Group("/api/v1/access/device", middleware.AuthMiddleware(c.Tokens), middleware.RequireOwner())
	{
		g.GET("", c.handleStatus)
		g.POST("/test-pulse", c.handleTestPulse)
	}
}

type testPulseRequest struct {
	DurationMS int64 `json:"duration_ms"`
}

type testPulseResponse struct {
	CommandID     string    `json:"command_id"`
	DeviceID      string    `json:"device_id"`
	Port          string    `json:"port"`
	State         string    `json:"state"`
	StartedAt     time.Time `json:"started_at"`
	ConfirmedAt   time.Time `json:"confirmed_at"`
	AuditRecorded bool      `json:"audit_recorded"`
}

func (c *AccessDeviceController) handleStatus(ctx *gin.Context) {
	status := c.Status.Execute(ctx.Request.Context())
	utils.JsonResponse(ctx, http.StatusOK, gin.H{
		"configured":      status.Configured,
		"connected":       status.Connected,
		"device_id":       status.DeviceID,
		"port":            status.Port,
		"available_ports": status.AvailablePorts,
		"protocol":        status.Protocol,
		"detail":          status.Detail,
	})
}

func (c *AccessDeviceController) handleTestPulse(ctx *gin.Context) {
	var req testPulseRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, err)
		return
	}
	if req.DurationMS == 0 {
		req.DurationMS = 1000
	}

	gymID, _ := middleware.GetGymID(ctx)
	ownerID, _ := middleware.GetUserID(ctx)
	out, err := c.Test.Execute(ctx.Request.Context(), chkApp.TestAccessActuatorInput{
		GymID:    gymID,
		OwnerID:  ownerID,
		Duration: time.Duration(req.DurationMS) * time.Millisecond,
	})
	if err != nil {
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}

	r := out.Receipt
	utils.JsonResponse(ctx, http.StatusOK, testPulseResponse{
		CommandID:     r.CommandID.String(),
		DeviceID:      r.DeviceID,
		Port:          r.Port,
		State:         r.State,
		StartedAt:     r.StartedAt,
		ConfirmedAt:   r.ConfirmedAt,
		AuditRecorded: out.AuditRecorded,
	})
}
