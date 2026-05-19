package controllers

import (
	"encoding/base64"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// FingerprintController handles UC-028 — POST /api/v1/members/:id/fingerprint
// and UC-028b — DELETE /api/v1/members/:id/fingerprint.
// Lives separately from MemberController so the route can be wired with the
// fingerprint use cases without having to bloat that constructor.
//
// Cloud + sidecar both expose the routes: cloud needs them for dashboard
// "manage fingerprint" actions (read/delete), sidecar needs them for the
// kiosko enroll flow. The use cases are the same; the GMK provider on each
// side decides where the encryption key comes from.
type FingerprintController struct {
	Register *memApp.RegisterFingerprint
	Delete   *memApp.DeleteFingerprint
	Tokens   auth.TokenService
}

func NewFingerprintController(register *memApp.RegisterFingerprint, deleteUC *memApp.DeleteFingerprint, tokens auth.TokenService) *FingerprintController {
	return &FingerprintController{Register: register, Delete: deleteUC, Tokens: tokens}
}

func (c *FingerprintController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(c.Tokens))
	{
		api.POST("/members/:id/fingerprint", c.handleRegister)
		api.DELETE("/members/:id/fingerprint", c.handleDelete)
	}
}

type registerFingerprintReq struct {
	// CaptureBase64 is the SDK-extracted plaintext template, base64-encoded.
	CaptureBase64   string `json:"capture_base64" binding:"required"`
	Format          string `json:"format,omitempty"`
	QualityScore    int    `json:"quality_score,omitempty"`
	ConsentAccepted bool   `json:"consent_accepted"`
}

type registerFingerprintResp struct {
	FingerprintID uuid.UUID `json:"fingerprint_id"`
	MemberID      uuid.UUID `json:"member_id"`
	QualityScore  *int      `json:"quality_score,omitempty"`
	RegisteredAt  string    `json:"registered_at"`
	Status        string    `json:"status"`
}

func (c *FingerprintController) handleRegister(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	actor, _ := middleware.GetUserID(ctx)
	memberID, ok := parseUUIDParam(ctx, "id")
	if !ok {
		return
	}
	var req registerFingerprintReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, err)
		return
	}
	bytes, err := base64.StdEncoding.DecodeString(req.CaptureBase64)
	if err != nil || len(bytes) == 0 {
		utils.ErrorResponse(ctx, http.StatusBadRequest, errors.New("capture_base64 inválido"))
		return
	}
	out, err := c.Register.Execute(ctx.Request.Context(), memApp.RegisterFingerprintInput{
		GymID: gymID, ActorUserID: actor, MemberID: memberID,
		Captures: []*biometric.CaptureResult{{
			Bytes: bytes, Format: req.Format, QualityScore: req.QualityScore,
		}},
		ConsentAccepted: req.ConsentAccepted,
	})
	if err != nil {
		if errors.Is(err, fpDomain.ErrFingerprintCollision) {
			c.writeCollision(ctx, err)
			return
		}
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(ctx, http.StatusCreated, registerFingerprintResp{
		FingerprintID: out.FingerprintIDs[0],
		MemberID:      out.MemberID,
		QualityScore:  out.QualityScore,
		RegisteredAt:  out.RegisteredAt.UTC().Format("2006-01-02T15:04:05Z"),
		Status:        "success",
	})
}

func (c *FingerprintController) handleDelete(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	actor, _ := middleware.GetUserID(ctx)
	memberID, ok := parseUUIDParam(ctx, "id")
	if !ok {
		return
	}
	err := c.Delete.Execute(ctx.Request.Context(), memApp.DeleteFingerprintInput{
		GymID: gymID, ActorUserID: actor, MemberID: memberID,
	})
	if err != nil {
		if errors.Is(err, fpDomain.ErrFingerprintNotFound) {
			utils.ErrorResponse(ctx, http.StatusNotFound, err)
			return
		}
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

// writeCollision emits the flat 409 body the desktop hook reads. The shape
// matches the duplicate-phone path on UC-012 so the FE can use one handler.
func (c *FingerprintController) writeCollision(ctx *gin.Context, err error) {
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
