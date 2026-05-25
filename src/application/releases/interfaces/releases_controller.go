// Package interfaces aloja el wire HTTP del módulo releases. Dos endpoints:
//
//   - GET  /api/v1/releases/latest/:target/:current_version
//   - POST /api/v1/admin/releases   (shared-secret auth)
//
// Mantener wire layer separado del use case (igual que el resto del repo)
// nos deja testear los use cases sin Gin y al revés.
package interfaces

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	releasesApp "github.com/cuadra/cuadra-core/src/application/releases"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// Controller bundlea ambos endpoints. AdminToken vacío deshabilita el POST
// admin (devuelve 503) — útil en entornos de desarrollo sin pipeline.
type Controller struct {
	GetLatest       *releasesApp.GetLatest
	RegisterRelease *releasesApp.RegisterRelease
	AdminToken      string
}

func NewController(get *releasesApp.GetLatest, register *releasesApp.RegisterRelease, adminToken string) *Controller {
	return &Controller{GetLatest: get, RegisterRelease: register, AdminToken: adminToken}
}

func (ctrl *Controller) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	// Public — Tauri updater. Sin auth: la firma ed25519 del payload
	// embebida en el binario es lo que cierra el threat model (ADR-005 §2.1).
	api.GET("/releases/latest/:target/:current_version", ctrl.handleGetLatest)
	// Admin — el pipeline llama acá tras publicar el manifest.
	api.POST("/admin/releases", ctrl.handleRegister)
}

// ---------------------------------------------------------------------------
// GET /releases/latest/:target/:current_version
// ---------------------------------------------------------------------------

// manifestWire es el shape que Tauri Updater 2 espera (ADR-005 §2.1). El
// campo `platforms` mapea target → {signature, url}. Para nuestro endpoint
// con el target en la URL, devolvemos UN solo entry — Tauri lo acepta igual.
type manifestWire struct {
	Version   string                  `json:"version"`
	Notes     string                  `json:"notes"`
	PubDate   time.Time               `json:"pub_date"`
	Platforms map[string]platformWire `json:"platforms"`
}

type platformWire struct {
	Signature string `json:"signature"`
	URL       string `json:"url"`
}

func (ctrl *Controller) handleGetLatest(c *gin.Context) {
	target := c.Param("target")
	currentVersion := c.Param("current_version")
	if target == "" || currentVersion == "" {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("target y current_version requeridos"))
		return
	}
	// Channel sale del query param ?channel= o default 'stable'. El selector
	// en UI llega post-v1 (ADR-005 §2.3) pero el campo vive desde día 1.
	channel := c.DefaultQuery("channel", "stable")
	if channel != "stable" && channel != "beta" {
		utils.ErrorResponse(c, http.StatusBadRequest, errors.New("channel inválido"))
		return
	}

	in := releasesApp.GetLatestInput{
		Target:         target,
		CurrentVersion: currentVersion,
		Channel:        channel,
	}
	// X-Tinta-Gym-ID es opcional. Si viene mal-formado, lo tratamos como
	// ausente — no merece 400, el updater puede no haber recibido el gym_id
	// todavía (sidecar pre-login) y igual queremos contestarle.
	if raw := strings.TrimSpace(c.GetHeader("X-Tinta-Gym-ID")); raw != "" {
		if gymID, err := uuid.Parse(raw); err == nil {
			in.GymID = &gymID
		}
	}

	out, err := ctrl.GetLatest.Execute(c.Request.Context(), in)
	if err != nil {
		utils.ErrorResponse(c, http.StatusInternalServerError, err)
		return
	}
	if out.NoUpdate || out.Release == nil {
		// 204: sin update para este cliente (ya está al día, fuera del bucket
		// de rollout, o target sin publicaciones). Tauri lo interpreta como
		// "no hay actualización" sin reintento agresivo.
		c.Status(http.StatusNoContent)
		return
	}
	rel := out.Release
	c.JSON(http.StatusOK, manifestWire{
		Version: rel.Version,
		Notes:   rel.Notes,
		PubDate: rel.PubDate,
		Platforms: map[string]platformWire{
			rel.TargetPlatform: {
				Signature: rel.SignatureEd25519,
				URL:       rel.URL,
			},
		},
	})
}

// ---------------------------------------------------------------------------
// POST /admin/releases  (shared-secret)
// ---------------------------------------------------------------------------

type registerWire struct {
	Version          string    `json:"version"`
	TargetPlatform   string    `json:"target_platform"`
	Channel          string    `json:"channel"`
	URL              string    `json:"url"`
	SignatureEd25519 string    `json:"signature_ed25519"`
	Notes            string    `json:"notes"`
	RolloutPercent   int       `json:"rollout_percent"`
	ForceImmediate   bool      `json:"force_immediate"`
	PubDate          time.Time `json:"pub_date"`
}

func (ctrl *Controller) handleRegister(c *gin.Context) {
	if ctrl.AdminToken == "" {
		utils.ErrorResponse(c, http.StatusServiceUnavailable,
			errors.New("admin releases endpoint deshabilitado (RELEASES_ADMIN_TOKEN no configurado)"))
		return
	}
	got := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(ctrl.AdminToken)) != 1 {
		utils.ErrorResponse(c, http.StatusUnauthorized, errors.New("token admin inválido"))
		return
	}

	var req registerWire
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(c, http.StatusBadRequest, err)
		return
	}
	rel, err := ctrl.RegisterRelease.Execute(c.Request.Context(), releasesApp.RegisterReleaseInput{
		Version:          req.Version,
		TargetPlatform:   req.TargetPlatform,
		Channel:          req.Channel,
		URL:              req.URL,
		SignatureEd25519: req.SignatureEd25519,
		Notes:            req.Notes,
		RolloutPercent:   req.RolloutPercent,
		ForceImmediate:   req.ForceImmediate,
		PubDate:          req.PubDate,
	})
	if err != nil {
		switch {
		case errors.Is(err, releasesApp.ErrInvalidVersion),
			errors.Is(err, releasesApp.ErrInvalidTarget),
			errors.Is(err, releasesApp.ErrInvalidURL),
			errors.Is(err, releasesApp.ErrInvalidSignature),
			errors.Is(err, releasesApp.ErrInvalidRollout),
			errors.Is(err, releasesApp.ErrInvalidChannel):
			utils.ErrorResponse(c, http.StatusBadRequest, err)
		default:
			utils.ErrorResponse(c, http.StatusInternalServerError, err)
		}
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":              rel.ID,
		"version":         rel.Version,
		"target_platform": rel.TargetPlatform,
		"channel":         rel.Channel,
		"rollout_percent": rel.RolloutPercent,
	})
}
