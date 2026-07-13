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
// for /sync/status.
//
// Earlier this function classified state purely off the gap from
// `LastSyncedAt`: "más de 5 min desde el último éxito" → `offline_medium`,
// which the FE renders como "Sin internet, todo guardado en esta laptop."
// Eso es incorrecto cuando NO hay falla real: el agente puede haber tickeado
// sin nada que empujar (push/pull no-ops actualizan LastSyncedAt sólo si
// hay actividad), el token puede no estar inyectado todavía, la laptop
// pudo haber dormido — ninguno de esos es "sin internet" desde la
// perspectiva del operador, y la alerta amarilla genera ansiedad
// innecesaria.
//
// Nueva semántica: `ConsecutiveFailures` y `LastError` son los signos
// primarios de "algo está mal". El gap se usa sólo como tiempo-de-vida
// final cuando ya pasó tanto que algo silencioso está roto (>24h).
//
//	verde      — agente saludable: hubo sync exitoso reciente y cero
//	             fallas consecutivas. También cubre el caso "agente
//	             idle sin nada que mandar" — no hay razón para
//	             pintar rojo/amarillo.
//	amarillo   — hubo intentos fallidos (consecutive_failures > 0) o
//	             pasaron >24h sin sincronizar.
//	rojo       — >7d sin sincronizar, o nunca sincronizó y ya hubo
//	             un intento fallido.
//	syncing    — full-sync inicial corriendo.
//
// Pulled out so tests can drive synthetic "now" without spinning up an
// agent.
func buildStatusResponse(snap AgentSnapshot, now time.Time) StatusResponse {
	r := StatusResponse{
		QueuePendingCount:     snap.PendingCount,
		LastError:             snap.LastError,
		ConsecutiveFailures:   snap.ConsecutiveFailures,
		InitialSyncDone:       !snap.InitialSyncCompletedAt.IsZero(),
		AuthInvalid:           snap.AuthInvalid || snap.WaitingForAuth,
		SchemaUpgradeRequired: snap.SchemaUpgradeRequired,
		LocalApplyError:       snap.LocalApplyError,
		QuarantinedCount:      snap.QuarantinedCount,
		QueueStuckCount:       snap.StuckPushCount,
	}
	// Los rechazos per-item viven en la FILA (sync_queue.last_error), no en
	// el LastError global (el ciclo HTTP fue "exitoso"). Si el global está
	// vacío pero hay filas atoradas, subimos el error de la más castigada —
	// sin esto el detalle decía "Último error: Ninguno" con 6 filas rotas.
	if r.LastError == "" && snap.StuckPushCount > 0 && snap.StuckPushError != "" {
		r.LastError = "push (fila atorada): " + snap.StuckPushError
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

	// SchemaUpgradeRequired tiene la máxima prioridad: aunque internet
	// funcione y la credencial sea válida, el cliente está roto hasta que
	// el binario se actualice. La UI muestra un modal bloqueante (no toast
	// discreto) — pintarlo como "online" o "offline_*" engañaría al
	// operador haciéndole pensar que el problema es de red.
	if snap.SchemaUpgradeRequired {
		r.State = StateSchemaUpgradeRequired
		return r
	}

	// Caso "initial syncing": full-sync corriendo todavía. Spinner.
	if snap.State == StateInitialSyncing && !r.InitialSyncDone {
		r.State = StateInitialSyncing
		return r
	}

	// La credencial del cloud está muerta (401). Tiene prioridad sobre
	// cualquier clasificación de "stale" — el operador necesita ver un
	// mensaje accionable ("vuelve a iniciar sesión"), no la genérica
	// "Sin internet".
	if snap.AuthInvalid {
		r.State = StateAuthInvalid
		return r
	}

	// El último fallo fue al APLICAR datos localmente (no de red), o hay
	// filas en cuarentena (saltadas tras fallar el umbral). El request
	// llegó y el cloud respondió; lo que rebotó fue el guardado local
	// (bug de esquema, CHECK, dato huérfano). Mostrarlo como "Sin
	// internet" mandaría al operador a revisar el router — el problema es
	// del sistema, no de la conexión. El conteo de cuarentena mantiene
	// este estado aunque el resto del sync fluya: saltar cambios nunca es
	// silencioso. Prioridad por encima de la clasificación offline.
	if snap.LocalApplyError || snap.QuarantinedCount > 0 || snap.StuckPushCount > 0 {
		r.State = StateSyncError
		return r
	}

	// Sidecar recién canjeado: nunca completó un sync. Sin error → spinner
	// (initial syncing). Con error → crítico (algo se rompió en el primer
	// intento).
	if snap.LastSyncedAt.IsZero() {
		if snap.LastError != "" {
			r.State = StateOfflineCritical
		} else {
			r.State = StateInitialSyncing
		}
		return r
	}

	gap := now.Sub(snap.LastSyncedAt)

	// Edge: pasó tanto que algo silencioso seguramente está roto. La alerta
	// vale la pena aunque consecutive_failures haya sido reseteado o el
	// agente esté pausado por falta de token. >7d siempre rojo; 24h..7d
	// amarillo de tipo "long".
	if gap >= 7*24*time.Hour {
		r.State = StateOfflineCritical
		return r
	}
	if gap >= 24*time.Hour {
		r.State = StateOfflineLong
		return r
	}

	// Hay fallas activas. Severidad escala con cuántas se acumulan y cuán
	// vieja es la última sync exitosa. Una o dos fallas transitorias se
	// muestran como "offline_short" (verde en el FE) — no asustar al
	// dueño por un blip pasajero. A partir de 3+ saltamos a medium.
	if snap.ConsecutiveFailures > 0 {
		if snap.ConsecutiveFailures < 3 && gap < 5*time.Minute {
			r.State = StateOfflineShort
		} else {
			r.State = StateOfflineMedium
		}
		return r
	}

	// Sin fallas, último éxito hace <24h. Estado saludable, incluso si la
	// última sync fue hace varios minutos: lo importante es que no haya
	// señal de problema. Si después aparece una falla real, este branch
	// no aplica.
	r.State = StateOnline
	return r
}
