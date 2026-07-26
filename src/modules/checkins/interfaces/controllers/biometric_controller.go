//go:build sidecar

package controllers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// BiometricController is the sidecar's FE-facing biometric surface after the
// tinta-bio inversion: los dedazos LLEGAN al sidecar desde el helper y el FE
// sólo escucha resultados. Cuatro rutas:
//
//   - GET  /api/v1/biometric/status        → {connected, available, ...}
//   - GET  /api/v1/biometric/events        → SSE: reader state, feedback de
//     sample, resultado de check-in (mismo wire CheckinEvent que los
//     endpoints), progreso/fin de enroll.
//   - POST /api/v1/biometric/enroll/start  → abre sesión de enroll {member_id}
//   - POST /api/v1/biometric/enroll/cancel → cancela la sesión activa
//
// Los endpoints multipart de imagen (PNG del FE) murieron con NBIS — la
// captura ya no cruza HTTP.
type BiometricController struct {
	Hub    *chkApp.BiometricHub
	Tokens auth.TokenService
}

func NewBiometricController(hub *chkApp.BiometricHub, tokens auth.TokenService) *BiometricController {
	return &BiometricController{Hub: hub, Tokens: tokens}
}

func (c *BiometricController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	api.Use(middleware.AuthMiddleware(c.Tokens))
	{
		api.GET("/biometric/status", c.handleStatus)
		api.GET("/biometric/events", c.handleEvents)
		api.POST("/biometric/enroll/start", c.handleEnrollStart)
		api.POST("/biometric/enroll/cancel", c.handleEnrollCancel)
	}
}

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// biometricStatusResp conserva el shape histórico {connected, available} que
// el FE ya consume; available = helper vivo + lector conectado.
type biometricStatusResp struct {
	DeviceID  string `json:"device_id"`
	Vendor    string `json:"vendor"`
	Model     string `json:"model"`
	Connected bool   `json:"connected"`
	Available bool   `json:"available"`
	Enrolling bool   `json:"enrolling"`
}

type enrollStartReq struct {
	MemberID        string `json:"member_id" binding:"required"`
	ConsentAccepted bool   `json:"consent_accepted"`
}

type enrollStartResp struct {
	SessionID       uuid.UUID `json:"session_id"`
	MemberID        uuid.UUID `json:"member_id"`
	ExpiresAt       string    `json:"expires_at"`
	RequiredSamples int       `json:"required_samples"`
}

// bioEventWire is the SSE `data:` payload. Type se repite adentro del JSON
// para que el FE pueda multiplexar con un solo onmessage si quiere.
type bioEventWire struct {
	Type      string            `json:"type"`
	Timestamp time.Time         `json:"timestamp"`
	Message   string            `json:"message,omitempty"`
	Code      string            `json:"code,omitempty"`
	Quality   string            `json:"quality,omitempty"`
	Reader    *readerWire       `json:"reader,omitempty"`
	Checkin   *checkinEventWire `json:"checkin,omitempty"`
	Enroll    *enrollEventWire  `json:"enroll,omitempty"`
}

type readerWire struct {
	Name   string `json:"name,omitempty"`
	Serial string `json:"serial,omitempty"`
}

type enrollEventWire struct {
	SessionID      string   `json:"session_id"`
	MemberID       string   `json:"member_id"`
	Captured       int      `json:"captured"`
	Required       int      `json:"required"`
	ExpiresAt      string   `json:"expires_at,omitempty"`
	FingerprintIDs []string `json:"fingerprint_ids,omitempty"`
	// Contrato de colisión (mismos campos que el 409 fingerprint_collision).
	ExistingMemberID   string `json:"existing_member_id,omitempty"`
	ExistingMemberName string `json:"existing_member_name,omitempty"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (c *BiometricController) handleStatus(ctx *gin.Context) {
	utils.JsonResponse(ctx, http.StatusOK, c.statusSnapshot())
}

func (c *BiometricController) statusSnapshot() biometricStatusResp {
	info := c.Hub.Engine.Info()
	return biometricStatusResp{
		DeviceID:  info.DeviceID,
		Vendor:    info.Vendor,
		Model:     info.Model,
		Connected: c.Hub.Engine.Connected(),
		Available: c.Hub.Available(),
		Enrolling: c.Hub.Enrolling(),
	}
}

func (c *BiometricController) handleEnrollStart(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	actor, _ := middleware.GetUserID(ctx)
	var req enrollStartReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, err)
		return
	}
	memberID, err := uuid.Parse(req.MemberID)
	if err != nil {
		utils.ErrorResponse(ctx, http.StatusBadRequest, errors.New("member_id inválido"))
		return
	}
	out, err := c.Hub.StartEnroll(ctx.Request.Context(), chkApp.StartEnrollInput{
		GymID:           gymID,
		ActorUserID:     actor,
		MemberID:        memberID,
		ConsentAccepted: req.ConsentAccepted,
	})
	if err != nil {
		if errors.Is(err, chkErrors.ErrEnrollSessionActive) {
			utils.ErrorResponse(ctx, http.StatusConflict, err)
			return
		}
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(ctx, http.StatusAccepted, enrollStartResp{
		SessionID:       out.SessionID,
		MemberID:        out.MemberID,
		ExpiresAt:       out.ExpiresAt.UTC().Format(time.RFC3339),
		RequiredSamples: out.RequiredSamples,
	})
}

func (c *BiometricController) handleEnrollCancel(ctx *gin.Context) {
	if err := c.Hub.CancelEnroll(ctx.Request.Context()); err != nil {
		if errors.Is(err, chkErrors.ErrEnrollSessionNotFound) {
			utils.ErrorResponse(ctx, http.StatusNotFound, err)
			return
		}
		utils.ErrorResponse(ctx, utils.DomainErrorToHttpCode(err), err)
		return
	}
	utils.JsonResponse(ctx, http.StatusOK, gin.H{"cancelled": true})
}

// handleEvents is the SSE stream. Auth middleware normal (mismo bearer que
// el resto de la API — el FE lo consume con fetch+ReadableStream, no con
// EventSource pelón). Cada evento va como `event: <type>` + `data: <json>`;
// un comentario `: hb` cada 15s mantiene vivos proxies y detecta clientes
// idos. Al conectar se manda un snapshot `status` para que el FE arranque
// sincronizado sin esperar el primer evento real.
func (c *BiometricController) handleEvents(ctx *gin.Context) {
	w := ctx.Writer
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	sub, cancel := c.Hub.Subscribe()
	defer cancel()

	writeSSE(w, "status", c.statusSnapshot())
	w.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	clientGone := ctx.Request.Context().Done()

	for {
		select {
		case <-clientGone:
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": hb\n\n")
			w.Flush()
		case evt, ok := <-sub:
			if !ok {
				return
			}
			writeSSE(w, string(evt.Type), toBioEventWire(evt))
			w.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// Wire mapping
// ---------------------------------------------------------------------------

func writeSSE(w gin.ResponseWriter, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

func toBioEventWire(evt chkApp.BioEvent) bioEventWire {
	out := bioEventWire{
		Type:      string(evt.Type),
		Timestamp: evt.Timestamp,
		Message:   evt.Message,
		Code:      evt.Code,
		Quality:   evt.Quality,
	}
	if evt.ReaderName != "" || evt.ReaderSerial != "" {
		out.Reader = &readerWire{Name: evt.ReaderName, Serial: evt.ReaderSerial}
	}
	if evt.Checkin != nil {
		w := toCheckinResp(evt.Checkin)
		out.Checkin = &w
	}
	if evt.Enroll != nil {
		e := &enrollEventWire{
			SessionID: evt.Enroll.SessionID.String(),
			MemberID:  evt.Enroll.MemberID.String(),
			Captured:  evt.Enroll.Captured,
			Required:  evt.Enroll.Required,
		}
		if !evt.Enroll.ExpiresAt.IsZero() {
			e.ExpiresAt = evt.Enroll.ExpiresAt.UTC().Format(time.RFC3339)
		}
		for _, id := range evt.Enroll.FingerprintIDs {
			e.FingerprintIDs = append(e.FingerprintIDs, id.String())
		}
		if v, ok := evt.Enroll.Data["existing_member_id"].(string); ok {
			e.ExistingMemberID = v
		}
		if v, ok := evt.Enroll.Data["existing_member_name"].(string); ok {
			e.ExistingMemberName = v
		}
		out.Enroll = e
	}
	return out
}
