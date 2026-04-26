//go:build sidecar

package controllers

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// kioskLongPollTimeout is how long the long-poll handler holds the request
// open before returning an empty array. The frontend re-issues immediately —
// 25s leaves margin under most load-balancer 30s read timeouts.
const kioskLongPollTimeout = 25 * time.Second

// KioskController is the sidecar-only set of endpoints that talk to the
// fingerprint reader directly: fingerprint checkin, biometric status, kiosk
// start/stop + event long-poll.
type KioskController struct {
	Fingerprint *chkApp.CheckinByFingerprint
	Loop        *chkApp.KioskLoop
	Events      *chkApp.KioskBroadcaster
	Reader      biometric.Reader
	Tokens      auth.TokenService
}

func NewKioskController(
	fingerprint *chkApp.CheckinByFingerprint,
	loop *chkApp.KioskLoop,
	events *chkApp.KioskBroadcaster,
	reader biometric.Reader,
	tokens auth.TokenService,
) *KioskController {
	return &KioskController{
		Fingerprint: fingerprint, Loop: loop, Events: events,
		Reader: reader, Tokens: tokens,
	}
}

func (c *KioskController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(c.Tokens))
	{
		api.GET("/biometric/status", c.handleStatus)
		api.POST("/checkins/fingerprint", c.handleFingerprintCheckin)
		api.POST("/kiosk/start", c.handleKioskStart)
		api.POST("/kiosk/stop", c.handleKioskStop)
		api.GET("/kiosk/events", c.handleKioskEvents)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

type biometricStatusResp struct {
	DeviceID  string `json:"device_id"`
	Vendor    string `json:"vendor"`
	Model     string `json:"model"`
	Connected bool   `json:"connected"`
	Available bool   `json:"available"`
}

type fingerprintCheckinReq struct {
	// CaptureBase64 is the SDK-extracted plaintext template, base64-encoded.
	// In the kiosk loop this never crosses HTTP — the loop calls the use case
	// directly. The endpoint exists for the (rare) "test from desktop UI"
	// flow and integration tests.
	CaptureBase64 string  `json:"capture_base64" binding:"required"`
	Format        string  `json:"format,omitempty"`
	QualityScore  int     `json:"quality_score,omitempty"`
	Threshold     float64 `json:"threshold,omitempty"`
}

type kioskStateResp struct {
	Running bool `json:"running"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (c *KioskController) handleStatus(ctx *gin.Context) {
	if c.Reader == nil {
		utils.JsonResponse(ctx, http.StatusOK, biometricStatusResp{Connected: false, Available: false})
		return
	}
	info := c.Reader.Info()
	utils.JsonResponse(ctx, http.StatusOK, biometricStatusResp{
		DeviceID:  info.DeviceID,
		Vendor:    info.Vendor,
		Model:     info.Model,
		Connected: info.Connected,
		Available: c.Reader.Available(ctx.Request.Context()),
	})
}

func (c *KioskController) handleFingerprintCheckin(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	var req fingerprintCheckinReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, err)
		return
	}
	bytes, err := decodeBase64(req.CaptureBase64)
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, err)
		return
	}
	out, err := c.Fingerprint.Execute(ctx.Request.Context(), chkApp.CheckinByFingerprintInput{
		GymID: gymID,
		Capture: &biometric.CaptureResult{
			Bytes: bytes, Format: req.Format, QualityScore: req.QualityScore,
		},
		Threshold: req.Threshold,
	})
	if err != nil {
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(ctx, http.StatusCreated, toCheckinResp(out))
}

func (c *KioskController) handleKioskStart(ctx *gin.Context) {
	if c.Loop == nil {
		utils.ErrorResponse(ctx, http.StatusServiceUnavailable, errors.New("kiosk loop no inicializado"))
		return
	}
	if err := c.Loop.Start(context.Background()); err != nil {
		utils.ErrorResponse(ctx, http.StatusInternalServerError, err)
		return
	}
	utils.JsonResponse(ctx, http.StatusOK, kioskStateResp{Running: true})
}

func (c *KioskController) handleKioskStop(ctx *gin.Context) {
	if c.Loop == nil {
		utils.JsonResponse(ctx, http.StatusOK, kioskStateResp{Running: false})
		return
	}
	c.Loop.Stop()
	utils.JsonResponse(ctx, http.StatusOK, kioskStateResp{Running: false})
}

// handleKioskEvents long-polls the broadcaster. Returns when (a) at least
// one event is queued, (b) the timeout fires (returns empty list — client
// re-polls), or (c) the request context cancels.
func (c *KioskController) handleKioskEvents(ctx *gin.Context) {
	sub, cancel := c.Events.Subscribe()
	defer cancel()

	timeout := time.NewTimer(kioskLongPollTimeout)
	defer timeout.Stop()

	events := make([]chkApp.KioskEvent, 0, 4)

	// Wait for at least one event, then drain whatever else is queued.
	select {
	case evt, ok := <-sub:
		if !ok {
			utils.JsonResponse(ctx, http.StatusOK, gin.H{"events": events})
			return
		}
		events = append(events, evt)
	case <-timeout.C:
		utils.JsonResponse(ctx, http.StatusOK, gin.H{"events": events})
		return
	case <-ctx.Request.Context().Done():
		utils.JsonResponse(ctx, http.StatusOK, gin.H{"events": events})
		return
	}

drain:
	for {
		select {
		case evt, ok := <-sub:
			if !ok {
				break drain
			}
			events = append(events, evt)
		default:
			break drain
		}
	}
	utils.JsonResponse(ctx, http.StatusOK, gin.H{"events": events})
}

// decodeBase64 wraps base64.StdEncoding.DecodeString with a friendlier error
// message — the kiosko frontend pastes raw SDK output and any encoding bug
// will surface here.
func decodeBase64(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("capture_base64 vacío")
	}
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("capture_base64 no es base64 válido")
	}
	return out, nil
}
