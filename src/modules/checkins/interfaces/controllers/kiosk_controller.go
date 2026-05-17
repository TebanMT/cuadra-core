//go:build sidecar

package controllers

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// KioskController is the sidecar-only set of endpoints that talk to the
// fingerprint reader directly: fingerprint checkin, biometric status, kiosk
// start/stop. The frontend drives the capture loop now (ADR-004-bis), so
// there's no /kiosk/events long-poll anymore — the BiometricController +
// JS SDK in cuadra-desktop publish events client-side.
type KioskController struct {
	Fingerprint *chkApp.CheckinByFingerprint
	Loop        *chkApp.KioskLoop
	Reader      biometric.Reader
	Tokens      auth.TokenService
	// Sibling controller — we call its dispatch helper after each
	// allowed fingerprint checkin so the access webhook fires from the
	// kiosk path too. Optional; nil → no-op.
	Sibling *CheckinController
}

func NewKioskController(
	fingerprint *chkApp.CheckinByFingerprint,
	loop *chkApp.KioskLoop,
	reader biometric.Reader,
	tokens auth.TokenService,
) *KioskController {
	return &KioskController{
		Fingerprint: fingerprint, Loop: loop,
		Reader: reader, Tokens: tokens,
	}
}

// WithSibling injects the regular CheckinController so the kiosk path can
// reuse its access-webhook dispatch logic. Builder-style; returns receiver.
func (c *KioskController) WithSibling(sibling *CheckinController) *KioskController {
	c.Sibling = sibling
	return c
}

func (c *KioskController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(c.Tokens))
	{
		api.GET("/biometric/status", c.handleStatus)
		api.POST("/checkins/fingerprint", c.handleFingerprintCheckin)
		api.POST("/kiosk/start", c.handleKioskStart)
		api.POST("/kiosk/stop", c.handleKioskStop)
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
	if c.Sibling != nil {
		c.Sibling.dispatchAccessWebhook(ctx, out)
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
