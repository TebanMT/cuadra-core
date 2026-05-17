//go:build sidecar

package controllers

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// maxFingerprintImageBytes caps the multipart `image` field. PNG captures
// from `@digitalpersona/fingerprint` are ~170 KB; 2 MB is a generous ceiling
// that still rejects anyone trying to upload a real photo by mistake.
const maxFingerprintImageBytes int64 = 2 << 20

// BiometricController exposes the ADR-004-bis HTTP surface — the sidecar
// receives raw PNG captures from the Tauri frontend (where the
// DigitalPersona JS SDK lives) and runs `mindtct` + the appropriate use
// case here.
//
// Two routes:
//   - POST /api/v1/biometric/enroll  → UC-028 (register fingerprint).
//   - POST /api/v1/biometric/checkin → UC-029 (1:N checkin).
//
// The existing JSON-body endpoints (POST /members/:id/fingerprint and
// POST /checkins/fingerprint) stay around for callers that already have a
// base64 template (kiosk loop, integration tests). These are additive.
type BiometricController struct {
	Reader  biometric.Reader
	Enroll  *memApp.RegisterFingerprint
	Checkin *chkApp.CheckinByFingerprint
	Tokens  auth.TokenService
	// Sibling lets the checkin path fire the access webhook on allowed
	// results — same wire as KioskController. Optional; nil → skip.
	Sibling *CheckinController
}

func NewBiometricController(
	reader biometric.Reader,
	enroll *memApp.RegisterFingerprint,
	checkin *chkApp.CheckinByFingerprint,
	tokens auth.TokenService,
) *BiometricController {
	return &BiometricController{
		Reader: reader, Enroll: enroll, Checkin: checkin, Tokens: tokens,
	}
}

// WithSibling wires the regular CheckinController so the biometric checkin
// path reuses its access-webhook dispatch (turnstile / cerradura).
func (c *BiometricController) WithSibling(s *CheckinController) *BiometricController {
	c.Sibling = s
	return c
}

func (c *BiometricController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(c.Tokens))
	{
		api.POST("/biometric/enroll", c.handleEnroll)
		api.POST("/biometric/checkin", c.handleCheckin)
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (c *BiometricController) handleEnroll(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	actor, _ := middleware.GetUserID(ctx)

	memberIDStr := ctx.PostForm("member_id")
	memberID, err := uuid.Parse(memberIDStr)
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, errors.New("member_id inválido"))
		return
	}
	consent := parseBool(ctx.PostForm("consent_accepted"))

	imageBytes, err := readImageField(ctx)
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, err)
		return
	}

	capture, err := c.Reader.ExtractTemplate(ctx.Request.Context(), imageBytes)
	if err != nil {
		utils.ErrorResponse(ctx, extractErrorCode(err), err)
		return
	}

	out, err := c.Enroll.Execute(ctx.Request.Context(), memApp.RegisterFingerprintInput{
		GymID:           gymID,
		ActorUserID:     actor,
		MemberID:        memberID,
		Capture:         capture,
		ConsentAccepted: consent,
	})
	if err != nil {
		if errors.Is(err, fpDomain.ErrFingerprintCollision) {
			writeCollisionResp(ctx, err)
			return
		}
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(ctx, http.StatusCreated, gin.H{
		"fingerprint_id": out.FingerprintID,
		"member_id":      out.MemberID,
		"quality_score":  out.QualityScore,
		"registered_at":  out.RegisteredAt.UTC().Format("2006-01-02T15:04:05Z"),
		"status":         "success",
	})
}

func (c *BiometricController) handleCheckin(ctx *gin.Context) {
	// gym_id defaults to the operator's active gym (JWT). The optional form
	// field is honored for the rare cross-gym tooling case; we still validate
	// it parses, but we don't enforce it matches the JWT — that's the
	// middleware's job once we add multi-gym operators.
	gymID, _ := middleware.GetGymID(ctx)
	if override := ctx.PostForm("gym_id"); override != "" {
		if parsed, err := uuid.Parse(override); err == nil {
			gymID = parsed
		}
	}

	imageBytes, err := readImageField(ctx)
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, err)
		return
	}

	capture, err := c.Reader.ExtractTemplate(ctx.Request.Context(), imageBytes)
	if err != nil {
		utils.ErrorResponse(ctx, extractErrorCode(err), err)
		return
	}

	view, err := c.Checkin.Execute(ctx.Request.Context(), chkApp.CheckinByFingerprintInput{
		GymID:   gymID,
		Capture: capture,
	})
	if err != nil {
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}
	if c.Sibling != nil {
		c.Sibling.dispatchAccessWebhook(ctx, view)
	}
	utils.JsonResponse(ctx, http.StatusCreated, toCheckinResp(view))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// readImageField pulls the multipart `image` field as raw bytes and rejects
// anything over maxFingerprintImageBytes. The Reader does the format-sniff;
// here we only care that *some* bytes arrived.
func readImageField(ctx *gin.Context) ([]byte, error) {
	if err := ctx.Request.ParseMultipartForm(maxFingerprintImageBytes); err != nil {
		return nil, errors.New("multipart inválido o demasiado grande")
	}
	file, header, err := ctx.Request.FormFile("image")
	if err != nil {
		return nil, errors.New("campo `image` requerido")
	}
	defer file.Close()
	if header != nil && header.Size > maxFingerprintImageBytes {
		return nil, errors.New("imagen demasiado grande")
	}
	bytes, err := io.ReadAll(io.LimitReader(file, maxFingerprintImageBytes+1))
	if err != nil {
		return nil, errors.New("no se pudo leer la imagen")
	}
	if len(bytes) == 0 {
		return nil, errors.New("imagen vacía")
	}
	if int64(len(bytes)) > maxFingerprintImageBytes {
		return nil, errors.New("imagen demasiado grande")
	}
	return bytes, nil
}

// extractErrorCode maps the sentinel errors returned by ExtractTemplate to
// HTTP status. Quality / no-finger errors are 400 (operator should retry);
// ErrNotAvailable is 503 (matcher not wired — NBIS binaries missing).
func extractErrorCode(err error) int {
	switch {
	case errors.Is(err, biometric.ErrNotAvailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, biometric.ErrQualityThreshold),
		errors.Is(err, biometric.ErrNoFingerDetected),
		errors.Is(err, biometric.ErrTemplateExtractFailed):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// writeCollisionResp mirrors fingerprint_controller.writeCollision so the
// desktop's collision modal works against either endpoint.
func writeCollisionResp(ctx *gin.Context, err error) {
	body := gin.H{
		"status_code": http.StatusConflict,
		"message":     "CONFLICT",
		"exception":   "fingerprint_collision",
	}
	var ce sharedDomain.CustomError
	if errors.As(err, &ce) {
		for k, v := range ce.Data {
			body[k] = v
		}
	}
	ctx.JSON(http.StatusConflict, body)
}

func parseBool(s string) bool {
	switch s {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
