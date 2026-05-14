//go:build sidecar

package sync

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// StatusController exposes /api/v1/sync/status on the sidecar so the
// frontend desktop UI can render the green/amber/red indicator (UC-044).
// No auth — sidecar is bound to 127.0.0.1 with the X-Local-Token gate
// already in main.go (ADR-003 §2.3).
type StatusController struct {
	Agent *Agent
}

func NewStatusController(a *Agent) *StatusController { return &StatusController{Agent: a} }

func (s *StatusController) RegisterRoutes(r *gin.Engine) {
	r.GET("/api/v1/sync/status", s.Get)
	r.POST("/api/v1/sync/trigger", s.Trigger)
	r.POST("/api/v1/sync/auth", s.SetAuth)
}

type authReq struct {
	Token string `json:"token"`
}

// SetAuth lets the desktop frontend hand the cloud JWT to the sidecar after
// login (UC-002). The agent doesn't share JWT secrets with the cloud — it
// just relays whatever the operator's login flow received. Empty token
// resets to "unauthenticated" (logout).
func (s *StatusController) SetAuth(c *gin.Context) {
	var req authReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if s.Agent != nil {
		// Persist alongside in-memory so the credential survives a sidecar
		// restart. Persist errors don't fail the call — the agent still has
		// the token in memory and the next /sync/auth will retry.
		_ = s.Agent.SetTokenAndPersist(c.Request.Context(), req.Token)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *StatusController) Get(c *gin.Context) {
	if s.Agent == nil {
		c.JSON(http.StatusOK, StatusResponse{State: StateOnline})
		return
	}
	snap := s.Agent.Snapshot()
	resp := buildStatusResponse(snap, time.Now().UTC())
	c.JSON(http.StatusOK, resp)
}

// Trigger lets the desktop UI ask for an immediate sync (for example from
// the "sync now" button in the connection dropdown).
func (s *StatusController) Trigger(c *gin.Context) {
	if s.Agent != nil {
		s.Agent.TriggerNow()
	}
	c.JSON(http.StatusAccepted, gin.H{"triggered": true})
}

// buildStatusResponse maps the in-memory AgentSnapshot to the wire shape
// and applies UC-044's threshold rules:
//
//	green  — last_synced_at within 5 min OR initial sync still running.
//	amber  — 5 min … 24 h since last_synced_at.
//	long   — 24 h … 7 d.
//	crit   — > 7 d.
//
// Pulled out so tests can drive synthetic "now" without spinning up an
// agent.
func buildStatusResponse(snap AgentSnapshot, now time.Time) StatusResponse {
	r := StatusResponse{
		QueuePendingCount:   snap.PendingCount,
		LastError:           snap.LastError,
		ConsecutiveFailures: snap.ConsecutiveFailures,
		InitialSyncDone:     !snap.InitialSyncCompletedAt.IsZero(),
	}
	if !snap.LastSyncedAt.IsZero() {
		t := snap.LastSyncedAt
		r.LastSyncedAt = &t
	}
	if !snap.LastPulledAt.IsZero() {
		t := snap.LastPulledAt
		r.LastPulledAt = &t
	}
	if !snap.NextRetryAt.IsZero() {
		t := snap.NextRetryAt
		r.NextRetryAt = &t
	}
	if snap.State == StateInitialSyncing && r.InitialSyncDone == false {
		r.State = StateInitialSyncing
		return r
	}
	if snap.LastSyncedAt.IsZero() {
		// Caso típico: el sidecar acaba de canjear el código de instalación
		// y todavía no completa su primera tanda de sync. Antes mapeábamos
		// esto a `offline_critical`, que en la UI sale como "Hay un
		// problema sincronizando" con tono rojo — alarmante para un dueño
		// recién instalado que no tiene NADA roto. Si hubo un intento y
		// falló, `LastError` lo refleja → ahí sí escalar a critical. Sin
		// error registrado, lo correcto es `initial_syncing` (spinner).
		if snap.LastError != "" {
			r.State = StateOfflineCritical
		} else {
			r.State = StateInitialSyncing
		}
		return r
	}
	gap := now.Sub(snap.LastSyncedAt)
	switch {
	case gap < 5*time.Minute:
		r.State = StateOnline
	case gap < 24*time.Hour:
		r.State = StateOfflineMedium
	case gap < 7*24*time.Hour:
		r.State = StateOfflineLong
	default:
		r.State = StateOfflineCritical
	}
	return r
}
