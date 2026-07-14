//go:build sidecar

package sync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// AgentConfig — knobs for the sidecar sync loop. ADR-001 §3.3/§3.4 fixes
// most of these; tests override Interval to drive the loop synchronously.
type AgentConfig struct {
	BaseURL          string        // e.g. "https://api.entinta.app"
	Interval         time.Duration // default 30s (DA-42.1)
	BatchSize        int           // default 100 (ADR §3.3)
	MaxBatchBytes    int           // default 1 MiB
	PullPageSize     int           // default 500
	FullSyncPageSize int           // default 1000 (DA-43.1)
	HTTPClient       *http.Client
	TokenProvider    func() string // returns a Bearer token; called on every request so refresh isn't our concern
	Logger           *log.Logger
	// UploadsDir es la raíz local en disco donde el agent cachea blobs
	// que la nube es canónica sobre (fotos de socios, futuro logo del
	// gym). El FE renderea imágenes vía la ruta local-serve del sidecar
	// que lee de aquí — nunca abre socket directo a R2. Vacío deshabilita
	// el cacheo local (uploads pendientes se quedan como data URLs).
	UploadsDir string
}

func defaultConfig() AgentConfig {
	return AgentConfig{
		Interval:         30 * time.Second,
		BatchSize:        100,
		MaxBatchBytes:    1 << 20,
		PullPageSize:     500,
		FullSyncPageSize: 1000,
		HTTPClient:       &http.Client{Timeout: 60 * time.Second},
	}
}

// Agent is the sidecar's sync loop. It owns the goroutine that periodically
// pushes pending sync_queue rows to the cloud and pulls fresh changes back.
//
// Lifecycle:
//
//	a := NewAgent(cfg, db, uow)
//	go a.Run(ctx)             // blocks until ctx cancelled
//	a.TriggerNow()            // ad-hoc kick (queue threshold, reconnect, …)
//	a.Snapshot()              // read-only state for /sync/status
//	cancel()                  // ctx cancellation drains and exits
type Agent struct {
	cfg AgentConfig
	db  *sqlx.DB
	uow sharedDomain.UnitOfWork

	mu       sync.RWMutex
	state    AgentSnapshot
	clientID uuid.UUID
	token    string
	// lastNoTokenLog rate-limits the "skipped — no token" warn so a sidecar
	// that boots before the desktop logs in doesn't spam stderr every 30s.
	// Guarded por mu igual que `token` para no requerir un mutex extra.
	lastNoTokenLog time.Time

	trigger chan struct{}
}

// SetToken stores the credential the agent uses for /sync/* requests. Empty
// token = unauthenticated (loop short-circuits). Llamado tanto por el
// endpoint deprecated /sync/auth como por el hook OnSidecarTokenChanged
// del proxy de auth tras un login/re-login exitoso.
//
// Un token no-vacío también limpia AuthInvalid: una credencial nueva
// invalida cualquier 401 previo (la credencial vieja se revocó cloud-side
// y la nueva acaba de mintarse).
func (a *Agent) SetToken(t string) {
	a.mu.Lock()
	a.token = t
	if t != "" {
		a.state.AuthInvalid = false
		a.state.WaitingForAuth = false
	}
	a.mu.Unlock()
	if t != "" {
		a.TriggerNow()
	}
}

func (a *Agent) currentToken() string {
	a.mu.RLock()
	t := a.token
	a.mu.RUnlock()
	if t != "" {
		return t
	}
	if a.cfg.TokenProvider != nil {
		return a.cfg.TokenProvider()
	}
	return ""
}

// AgentSnapshot is the current state visible to /sync/status. We hold this
// in memory so the status endpoint never has to take a DB read on the hot
// path (status is polled every few seconds by the desktop UI).
type AgentSnapshot struct {
	LastPushedAt           time.Time
	LastPulledAt           time.Time
	LastSyncedAt           time.Time
	LastError              string
	PendingCount           int
	ConsecutiveFailures    int
	NextRetryAt            time.Time
	InitialSyncCompletedAt time.Time
	State                  string
	// AuthInvalid — true cuando el cloud rechazó la credencial del sidecar
	// con 401. Distingue una falla de auth ("revoca la credencial vieja,
	// hace falta re-login") de una falla de red/HTTP genérica. Se limpia
	// al recibir un nuevo token vía SetToken o tras un éxito.
	AuthInvalid bool
	// WaitingForAuth — true cuando RunOnce salteó por no tener token aún.
	// Útil para distinguir "sidecar recién canjeado, esperando login" de
	// "agente sano sin actividad reciente". Se limpia tan pronto como el
	// agente recibe credenciales.
	WaitingForAuth bool
	// SchemaUpgradeRequired — true cuando el cloud devolvió 426 a un
	// push/pull/full-sync. Significa que el wire-protocol que habla este
	// sidecar quedó atrás (ADR-001 §3.8). El usuario tiene que actualizar
	// el binario; los retries no resuelven nada. La UI muestra un modal
	// bloqueante con CTA "Actualizar ahora" (ADR-005 §2.7). Se limpia
	// recién cuando un sync exitoso confirma que la nueva versión sí
	// negocia el schema (= update aplicado).
	SchemaUpgradeRequired bool
	// LocalApplyError — true cuando el último fallo fue al APLICAR datos
	// localmente (ErrLocalApply), no de red. Refleja el ÚLTIMO intento:
	// se prende con un fallo de aplicación y se apaga con un fallo de red
	// o un éxito. La UI lo muestra como "Hay un problema al guardar
	// cambios" (accionable: actualizar/reportar), NO como "Sin internet".
	LocalApplyError bool
	// QuarantinedCount — filas que el pull SALTÓ tras fallar el umbral de
	// intentos (ver agent_quarantine.go). >0 significa "sync fluye pero
	// hay N cambios que no pudieron aplicarse" — el estado lo muestra como
	// StateSyncError para que saltar nunca sea silencioso.
	QuarantinedCount int
	// StuckPushCount — filas de sync_queue que el server rechazó per-item
	// N+ veces (retry_count >= stuckPushThreshold) mientras el CICLO
	// global "tiene éxito" (HTTP 200 con statuses de rechazo por fila).
	// Sin esto el indicador decía "Sincronizado" con la cola pudriéndose
	// — caso real post-migración: 6 filas de socios pre-corte rechazadas
	// por FKs contra un mundo que el wipe borró. StuckPushError guarda el
	// last_error (ya clasificado/limpio) de la fila más castigada para que
	// el detalle del indicador diga POR QUÉ sin abrir sqlite.
	StuckPushCount int
	StuckPushError string
	// StuckItems — detalle de las filas atoradas (cap a
	// maxStuckItemsInStatus, retry_count DESC). Cada una viene clasificada
	// (kind duplicate/other) y con label humano extraído del payload, para
	// que /sync/status pueda ofrecer resolución por fila en el desktop.
	// Tratado como INMUTABLE una vez publicado: refreshPendingCount siempre
	// asigna un slice fresco, nunca muta el existente.
	StuckItems []StuckQueueItem
}

func NewAgent(cfg AgentConfig, db *sqlx.DB, uow sharedDomain.UnitOfWork) *Agent {
	full := defaultConfig()
	if cfg.Interval > 0 {
		full.Interval = cfg.Interval
	}
	if cfg.BatchSize > 0 {
		full.BatchSize = cfg.BatchSize
	}
	if cfg.MaxBatchBytes > 0 {
		full.MaxBatchBytes = cfg.MaxBatchBytes
	}
	if cfg.PullPageSize > 0 {
		full.PullPageSize = cfg.PullPageSize
	}
	if cfg.FullSyncPageSize > 0 {
		full.FullSyncPageSize = cfg.FullSyncPageSize
	}
	if cfg.BaseURL != "" {
		full.BaseURL = cfg.BaseURL
	}
	if cfg.HTTPClient != nil {
		full.HTTPClient = cfg.HTTPClient
	}
	full.UploadsDir = cfg.UploadsDir
	full.TokenProvider = cfg.TokenProvider
	full.Logger = cfg.Logger
	if full.Logger == nil {
		full.Logger = log.New(io.Discard, "", 0)
	}
	return &Agent{
		cfg:     full,
		db:      db,
		uow:     uow,
		trigger: make(chan struct{}, 1),
	}
}

// TriggerNow nudges the loop to run immediately (drops the nudge if one is
// already queued — coalesces fast bursts).
func (a *Agent) TriggerNow() {
	select {
	case a.trigger <- struct{}{}:
	default:
	}
}

// Snapshot returns the current in-memory state for /sync/status.
func (a *Agent) Snapshot() AgentSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	snap := a.state
	// Copia defensiva del slice: el struct se copia por valor pero el
	// backing array sería compartido — un refresh concurrente publica un
	// slice nuevo (no muta), aun así el copy mantiene la garantía sin
	// depender de esa disciplina.
	if len(a.state.StuckItems) > 0 {
		snap.StuckItems = append([]StuckQueueItem(nil), a.state.StuckItems...)
	}
	return snap
}

// Run is the main agent loop. Blocks until ctx is cancelled. Safe to call
// once per process; calling twice would race the DB writes.
func (a *Agent) Run(ctx context.Context) {
	if a.cfg.BaseURL == "" {
		a.cfg.Logger.Printf("[sync] agent disabled — BaseURL empty")
		return
	}
	// Log absoluto del UploadsDir al boot. Esto deja un rastro visible
	// para verificar que las fotos están cayendo en un directorio
	// persistente (p.ej. ~/Library/Application Support/<APP>/uploads en
	// macOS) y NO en algo bajo /tmp o relativo al cwd, que se borra al
	// reiniciar. Si está vacío, las fotos no se cachean en disco — el
	// FE no podrá renderear via local-serve.
	if a.cfg.UploadsDir == "" {
		a.cfg.Logger.Printf("[sync] WARN: UploadsDir vacío — las fotos no se cachearán localmente")
	} else if abs, err := filepath.Abs(a.cfg.UploadsDir); err == nil {
		a.cfg.Logger.Printf("[sync] uploads cache dir: %s", abs)
		if strings.HasPrefix(abs, "/tmp/") || strings.Contains(abs, "/tmp/") {
			a.cfg.Logger.Printf("[sync] WARN: UploadsDir bajo /tmp — se borrará al reiniciar el OS")
		}
	}
	if err := a.bootstrap(ctx); err != nil {
		a.cfg.Logger.Printf("[sync] bootstrap: %v", err)
	}
	ticker := time.NewTicker(a.cfg.Interval)
	defer ticker.Stop()
	// Run once on start.
	a.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.RunOnce(ctx)
		case <-a.trigger:
			a.RunOnce(ctx)
		}
	}
}

// RunOnce — single iteration of the loop (full-sync if needed → push → pull).
// Surfaces errors to the in-memory snapshot but doesn't return them; the
// loop is best-effort.
func (a *Agent) RunOnce(ctx context.Context) {
	if a.currentToken() == "" {
		// Sin credencial. Antes esto retornaba silencioso, lo que dejaba
		// al sidecar en un limbo "sano pero parado" imposible de
		// diagnosticar (sin error, sin sync, sin log). Ahora marcamos
		// WaitingForAuth para que /sync/status lo distinga, y emitimos
		// un warn rate-limited cada 5 min para que op-quality alerte
		// si el sidecar lleva horas sin token.
		a.mu.Lock()
		a.state.WaitingForAuth = true
		shouldLog := time.Since(a.lastNoTokenLog) > 5*time.Minute
		if shouldLog {
			a.lastNoTokenLog = time.Now()
		}
		a.mu.Unlock()
		if shouldLog {
			a.cfg.Logger.Printf("[sync] WARN: skipping run — no credential (waiting for login)")
		}
		return
	}
	// Si llegamos acá tenemos token: limpiamos la marca de "esperando
	// login". AuthInvalid lo limpia SetToken explícitamente al hot-swap;
	// no lo tocamos acá para no enmascarar un 401 todavía sin recambio.
	a.mu.Lock()
	a.state.WaitingForAuth = false
	a.mu.Unlock()
	// If we're inside a backoff window, skip.
	a.mu.RLock()
	wait := a.state.NextRetryAt
	a.mu.RUnlock()
	if !wait.IsZero() && time.Now().Before(wait) {
		return
	}

	// Full sync first if needed (only runs once per device install).
	if !a.initialSyncDone() {
		if err := a.FullSync(ctx); err != nil {
			a.recordFailure(ctx, "full-sync", err)
			return
		}
	}

	// Sube fotos pendientes a R2 ANTES del push: cualquier upload
	// exitoso bumpea version + encola el row, así el push siguiente
	// propaga el object_key en lugar del data: URL local. Errores
	// de upload no detienen sync — se loggean y reintenta el siguiente
	// tick. Sidecar-only (la función vive en agent_uploads.go con tag
	// //go:build sidecar). Tanto fotos de socios (members.photo_url)
	// como de productos (products.image_url) siguen el mismo ciclo.
	a.uploadPendingMemberPhotos(ctx)
	a.uploadPendingProductPhotos(ctx)

	if err := a.Push(ctx); err != nil {
		a.recordFailure(ctx, "push", err)
		return
	}
	if err := a.Pull(ctx); err != nil {
		a.recordFailure(ctx, "pull", err)
		return
	}
	// Baja a disco las fotos cuyas filas locales tienen un object_key R2
	// pero el archivo aún no existe en cache. Cubre el caso multi-device
	// (gym con dos desktops) y rehidratación post full-sync. Errores
	// no detienen el ciclo — el archivo se reintentará en el próximo
	// tick. Sidecar-only (definido con //go:build sidecar).
	a.downloadPendingMemberPhotos(ctx)
	a.downloadPendingProductPhotos(ctx)
	a.recordSuccess(ctx)
}

// Bootstrap forces a fresh read of sync_state into the in-memory snapshot.
// Run() calls this on startup; tests call it directly after seeding state
// so RunOnce sees up-to-date initial_sync flags.
func (a *Agent) Bootstrap(ctx context.Context) error { return a.bootstrap(ctx) }

func (a *Agent) bootstrap(ctx context.Context) error {
	return a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		id, err := EnsureClientID(ctx, tx)
		if err != nil {
			return err
		}
		st, err := ReadState(ctx, tx)
		if err != nil {
			return err
		}
		a.clientID = id
		a.mu.Lock()
		a.state = AgentSnapshot{
			LastPulledAt:           st.LastPulledAt,
			LastSyncedAt:           st.LastSyncedAt,
			ConsecutiveFailures:    st.ConsecutiveFailures,
			InitialSyncCompletedAt: st.InitialSyncCompletedAt,
			// NextRetryAt NO se restaura a propósito: el backoff persistido
			// lo calculó un proceso anterior — posiblemente OTRO binario
			// (auto-update) cuyo fallo el nuevo justamente arregla. Un
			// arranque fresco merece un intento inmediato; si el fallo
			// persiste, ConsecutiveFailures (que sí se restaura) hace que
			// recordFailure re-arme el backoff directo en el cap, sin
			// martillar desde 1s. Bug real del dogfood: tras actualizar
			// v1.0.6→v1.0.7 el sidecar quedó mudo hasta 5 min "esperando"
			// un retry heredado, sin nada en la UI que lo explicara.
		}
		// Restore the persisted Bearer credential so the agent can resume
		// syncing after a sidecar restart without waiting for the desktop
		// to re-push it. The desktop pushes again whenever its access token
		// rotates (login / refresh / hydrate), keeping this row fresh.
		if st.SidecarToken != "" {
			a.token = st.SidecarToken
		}
		// Restaura el conteo de cuarentena para que, tras un restart, el
		// estado refleje filas ya saltadas (y quarantineNonEmpty gatee el
		// clear-on-apply). Best-effort dentro del bootstrap tx.
		if n, err := countQuarantined(ctx, tx.(*sharedDomain.SqlxTransaction)); err == nil {
			a.state.QuarantinedCount = n
		}
		a.mu.Unlock()
		return nil
	})
}

// SetTokenAndPersist updates the in-memory token AND writes it to sync_state
// so it survives a sidecar restart. Empty token clears both. Falls back to
// in-memory only if the persist write fails — the agent stays usable while
// next bootstrap re-tries the read.
func (a *Agent) SetTokenAndPersist(ctx context.Context, t string) error {
	a.SetToken(t)
	return a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		return SetSidecarToken(ctx, tx, t)
	})
}

func (a *Agent) initialSyncDone() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.state.InitialSyncCompletedAt.IsZero()
}

// ── Push ────────────────────────────────────────────────────────────────

// Push drains pending sync_queue rows in batches and applies the server's
// per-item results. Returns nil if there's nothing to do or every batch
// completed; returns the first transport-level error otherwise.
func (a *Agent) Push(ctx context.Context) error {
	for {
		batch, err := a.takeBatch(ctx)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			a.refreshPendingCount(ctx)
			return nil
		}
		req := PushRequest{
			ClientID:      a.clientID.String(),
			ClientNow:     time.Now().UTC(),
			SchemaVersion: SchemaVersion,
			Batch:         batch,
		}
		resp, err := a.postPush(ctx, req)
		if err != nil {
			return err
		}
		if err := a.handlePushResponse(ctx, batch, resp); err != nil {
			// El write-back de un push aceptado (stamp local / server-wins)
			// es aplicación local, no red — mismo trato de estado que el
			// pull para no mentir con "Sin internet".
			return localApplyErr(err)
		}
		a.markPushedAt(time.Now().UTC())
		// progress = el server resolvió AL MENOS un item (accepted o
		// conflict_*): esas filas ya tienen synced_at y salen de la cola.
		// Sin progreso en un batch COMPLETO, re-tomar la cola devolvería
		// exactamente el mismo batch (synced_at sigue NULL, mismo orden por
		// rowid) → loop caliente infinito martillando al cloud dentro de un
		// solo RunOnce. Con >= BatchSize filas todas atoradas (p.ej. una
		// cola envejecida de rechazos permanentes) hay que salir y dejar
		// que el próximo tick reintente como cualquier rechazo.
		progress := false
		for _, r := range resp.Results {
			switch r.Status {
			case StatusAccepted, StatusConflictClientWins, StatusConflictServerWins:
				progress = true
			}
		}
		if len(batch) < a.cfg.BatchSize || !progress {
			a.refreshPendingCount(ctx)
			return nil
		}
	}
}

func (a *Agent) takeBatch(ctx context.Context) ([]PushItem, error) {
	tx, err := a.uow.Query(ctx)
	if err != nil {
		return nil, err
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	// ORDER BY rowid — respeta el orden de inserción de cada Enqueue
	// call. Esto es load-bearing: dos rows enqueued en la misma
	// transacción (ej. CreateMember encola member + membership)
	// comparten enqueued_at hasta el milisegundo, así que ese campo
	// no rompe el empate. El secundario era `id ASC` (UUID aleatorio)
	// y a veces ponía el child antes del parent → FK violation en
	// cloud (memberships.member_id_fkey).
	//
	// El rowid implícito de SQLite es monotónico, asignado en orden
	// de INSERT, y el coalescing UPDATE no lo muta — por eso es
	// estable frente a re-enqueues (ej. el sync agent re-encolando
	// member con photo_url tras subir a R2).
	const q = `
		SELECT id, entity_type, entity_id, operation, payload, client_version, enqueued_at
		FROM sync_queue
		WHERE synced_at IS NULL
		ORDER BY rowid ASC
		LIMIT ?`
	rows, err := stx.Ext().QueryxContext(ctx, q, a.cfg.BatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PushItem, 0, a.cfg.BatchSize)
	bytesUsed := 0
	for rows.Next() {
		var (
			id, entityType, entityID, op, payload string
			clientVersion                         int
			enqueuedAt                            int64
		)
		if err := rows.Scan(&id, &entityType, &entityID, &op, &payload, &clientVersion, &enqueuedAt); err != nil {
			return nil, err
		}
		bytesUsed += len(payload) + 64 // rough envelope overhead
		if bytesUsed > a.cfg.MaxBatchBytes && len(out) > 0 {
			break
		}
		out = append(out, PushItem{
			QueueID:       id,
			EntityType:    entityType,
			EntityID:      entityID,
			Operation:     op,
			ClientVersion: clientVersion,
			Payload:       json.RawMessage(payload),
			EnqueuedAt:    time.UnixMilli(enqueuedAt).UTC(),
		})
	}
	return out, nil
}

func (a *Agent) postPush(ctx context.Context, req PushRequest) (*PushResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	endpoint, err := url.JoinPath(a.cfg.BaseURL, "/api/v1/sync/push")
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.currentToken())
	resp, err := a.cfg.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUpgradeRequired {
		return nil, ErrSchemaUpgradeRequired
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("push 5xx: %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("push %d: %s", resp.StatusCode, body)
	}
	var pr PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

func (a *Agent) handlePushResponse(ctx context.Context, batch []PushItem, resp *PushResponse) error {
	itemByQID := make(map[string]PushItem, len(batch))
	for _, it := range batch {
		itemByQID[it.QueueID] = it
	}
	return a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		// Un batch con varios server_wins puede traer una cadena FK
		// intra-tipo en orden adverso (misma razón que ApplyPullPage) —
		// diferir las FKs al COMMIT la tolera. Per-transaction, sin efecto
		// fuera de esta tx.
		if _, err := stx.Exec(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
			return fmt.Errorf("defer_foreign_keys: %w", err)
		}
		nowMs := time.Now().UTC().UnixMilli()
		for _, r := range resp.Results {
			orig := itemByQID[r.QueueID]
			switch r.Status {
			case StatusAccepted, StatusConflictClientWins:
				if _, err := stx.Exec(ctx, `
					UPDATE sync_queue SET synced_at = ?, last_error = NULL
					WHERE id = ?`, nowMs, r.QueueID); err != nil {
					return err
				}
				if r.ServerVersion > 0 && r.ServerUpdatedAt != nil {
					if err := stampLocalSynced(ctx, stx, orig, r.ServerVersion, r.ServerUpdatedAt.UnixMilli()); err != nil {
						return err
					}
				}
				if r.Status == StatusConflictClientWins {
					_ = recordLocalConflict(ctx, stx, orig, r, "client_wins")
				}
			case StatusConflictServerWins:
				if _, err := stx.Exec(ctx, `
					UPDATE sync_queue SET synced_at = ?, last_error = NULL
					WHERE id = ?`, nowMs, r.QueueID); err != nil {
					return err
				}
				// Apply the server's canonical state to local.
				if len(r.ServerPayload) > 0 && r.ServerUpdatedAt != nil {
					change := PullChange{
						EntityType:      orig.EntityType,
						EntityID:        orig.EntityID,
						Version:         r.ServerVersion,
						Payload:         r.ServerPayload,
						ServerUpdatedAt: *r.ServerUpdatedAt,
					}
					if err := ApplyPullChange(ctx, tx, change); err != nil {
						return fmt.Errorf("apply %s/%s (version %d): %w",
							change.EntityType, change.EntityID, change.Version, err)
					}
				}
				_ = recordLocalConflict(ctx, stx, orig, r, "server_wins")
			case StatusRejectedUnauthorized,
				StatusRejectedSchema,
				StatusRejectedUnknownType,
				StatusRejectedDuplicate:
				// Hard-rejected — keep the queue row, bump retry_count, log.
				// Operator's UI will eventually surface this via /sync/status.
				// El prefijo de status en last_error es load-bearing para
				// rejected_duplicate: classifyStuckError lo usa para marcar
				// la fila como kind=duplicate y que el desktop ofrezca el
				// CTA de renombrar (el mensaje ya viene legible del cloud).
				if _, err := stx.Exec(ctx, `
					UPDATE sync_queue SET retry_count = retry_count + 1, last_error = ?
					WHERE id = ?`, r.Status+": "+r.Error, r.QueueID); err != nil {
					return err
				}
			default:
				if _, err := stx.Exec(ctx, `
					UPDATE sync_queue SET retry_count = retry_count + 1, last_error = ?
					WHERE id = ?`, r.Error, r.QueueID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func stampLocalSynced(ctx context.Context, stx *sharedDomain.SqlxTransaction, orig PushItem, serverVersion int, serverUpdatedMs int64) error {
	t := FindTable(orig.EntityType)
	if t == nil {
		return nil
	}
	// Las tablas de llave compuesta (owner_alert_configs) no tienen columna
	// `id`; se ubican por su llave natural leída del payload que ya se
	// pusheó. Ver localKeyPredicate — sin esto, el stamp reventaba con
	// "no such column: id" y hacía rollback de TODO el batch (el error que
	// vio el operador al crear una promo/socio con un toggle de alerta
	// atascado en la cola).
	var pl map[string]any
	if len(orig.Payload) > 0 {
		_ = json.Unmarshal(orig.Payload, &pl)
	}
	whereClause, whereArgs := localKeyPredicate(t, orig.EntityID, pl)
	stmt := fmt.Sprintf(`UPDATE %s SET synced_at = ?, version = ?, updated_at = ? WHERE %s`, t.Table, whereClause)
	args := append([]any{time.Now().UTC().UnixMilli(), serverVersion, serverUpdatedMs}, whereArgs...)
	_, err := stx.Exec(ctx, stmt, args...)
	return err
}

func recordLocalConflict(ctx context.Context, stx *sharedDomain.SqlxTransaction, orig PushItem, r PushItemResult, resolution string) error {
	gymID := extractGymIDFromPayload(orig.Payload)
	_, err := stx.Exec(ctx, `
		INSERT INTO conflict_log_local
		    (gym_id, entity_type, entity_id, client_version, server_version,
		     client_payload, server_payload, resolution, detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gymID, orig.EntityType, orig.EntityID, orig.ClientVersion, r.ServerVersion,
		string(orig.Payload), string(coalesce(r.ServerPayload)), resolution, time.Now().UTC().UnixMilli(),
	)
	return err
}

func coalesce(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(`{}`)
	}
	return b
}

func extractGymIDFromPayload(p json.RawMessage) string {
	var pl map[string]any
	if err := json.Unmarshal(p, &pl); err != nil {
		return ""
	}
	if s, ok := pl["gym_id"].(string); ok {
		return s
	}
	return ""
}

// ── Pull ────────────────────────────────────────────────────────────────

// Pull pages through the cloud's incremental change feed since
// `last_pulled_at`. Updates state only after a full page commits — if we
// crash mid-page, next pull re-fetches (idempotent).
func (a *Agent) Pull(ctx context.Context) error {
	a.mu.RLock()
	since := a.state.LastPulledAt
	a.mu.RUnlock()
	// pending acumula páginas cuyo COMMIT falló por una cadena FK partida
	// justo en el límite de página (p.ej. la membresía vieja con
	// replaced_by al final de la página N, la nueva al inicio de la N+1 —
	// improbable con page size real, pero determinista si pasa: reintentar
	// la misma página sola re-lee el mismo corte y vuelve a fallar). En
	// ese caso NO avanzamos last_pulled_at (la tx rollbackeó) y
	// reintentamos la página UNIDA con la siguiente: con FKs diferidas
	// (ApplyPullPage) el commit del lote unido cierra la cadena.
	var pending []PullChange
	for {
		resp, err := a.getPull(ctx, since)
		if err != nil {
			return err
		}
		if len(resp.Changes) == 0 && len(pending) == 0 {
			return nil
		}
		pending = append(pending, resp.Changes...)
		batch := pending
		advanceCursor := func(tx sharedDomain.Transaction) error {
			return SetLastPulledAt(ctx, tx, batch[len(batch)-1].ServerUpdatedAt)
		}
		applyErr := ApplyPullPage(ctx, a.uow, batch, advanceCursor)
		if applyErr != nil {
			// El guard len(resp.Changes) > 0 evita loop infinito si el
			// server reporta has_more con página vacía (since no avanzaría).
			if isFKConstraintErr(applyErr) && resp.HasMore && len(resp.Changes) > 0 {
				since = resp.Changes[len(resp.Changes)-1].ServerUpdatedAt
				continue
			}
			if isFKConstraintErr(applyErr) {
				// FK rota de verdad (el padre no existe en TODO el feed).
				// SQLite no dice qué fila — el diagnóstico sí.
				return localApplyErr(fmt.Errorf("%w — %s", applyErr, describeFKViolations(ctx, a.uow, batch)))
			}
			// Error de aplicación NO-FK: una fila no se puede aplicar (bug de
			// esquema, CHECK, dato imposible). En vez de wedge-ear TODO el
			// sync para siempre, intentamos aislarla y saltarla tras el
			// umbral de intentos (self-heal). Ver agent_quarantine.go.
			handled, qErr := a.quarantineAndRetry(ctx, batch, advanceCursor)
			if qErr != nil {
				return localApplyErr(qErr)
			}
			if !handled {
				// Aún no toca saltar (veneno bajo umbral, o no aislable):
				// devolver el error → backoff → reintento. El estado ya
				// queda marcado como local-apply (no "Sin internet").
				return localApplyErr(applyErr)
			}
			// Saltamos el veneno y la página limpia aterrizó: el cursor ya
			// avanzó (advanceCursor corrió dentro del re-apply). Seguimos
			// como si hubiera sido un apply normal.
			a.refreshQuarantinedCount(ctx)
		} else if a.quarantineNonEmpty() {
			// Apply normal exitoso: si había cuarentena, alguna de estas
			// filas pudo haberse curado (el cloud subió su version con el
			// dato corregido) — límpialas para que el conteo baje.
			_ = a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
				return clearQuarantineForApplied(ctx, tx.(*sharedDomain.SqlxTransaction), batch)
			})
			a.refreshQuarantinedCount(ctx)
		}
		newSince := batch[len(batch)-1].ServerUpdatedAt
		pending = nil
		a.mu.Lock()
		a.state.LastPulledAt = newSince
		a.mu.Unlock()
		since = newSince
		if !resp.HasMore {
			return nil
		}
	}
}

// quarantineNonEmpty — lectura barata del conteo en memoria, para gatear
// el clear-on-apply sólo cuando hay algo en cuarentena (el install sano no
// paga el DELETE por página).
func (a *Agent) quarantineNonEmpty() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.state.QuarantinedCount > 0
}

func (a *Agent) getPull(ctx context.Context, since time.Time) (*PullResponse, error) {
	endpoint, err := url.JoinPath(a.cfg.BaseURL, "/api/v1/sync/pull")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339Nano))
	}
	q.Set("limit", strconv.Itoa(a.cfg.PullPageSize))
	endpoint += "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.currentToken())
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUpgradeRequired {
		return nil, ErrSchemaUpgradeRequired
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("pull 5xx: %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pull %d: %s", resp.StatusCode, body)
	}
	var pr PullResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// ── Full sync ────────────────────────────────────────────────────────────

// FullSync downloads every entity for this gym in topological order. The
// cursor persists between pages so a crash mid-restore picks up where it
// left off (ADR-001 §3.5 step 5).
func (a *Agent) FullSync(ctx context.Context) error {
	cursor, err := a.loadFullCursor(ctx)
	if err != nil {
		return err
	}
	a.setStateLabel(StateInitialSyncing)
	// pending: mismo mecanismo anti "cadena FK partida entre páginas" que
	// Pull — ver el comentario ahí. Acá el cursor avanza sin persistirse
	// (SetFullSyncCursor rollbackeó junto con la página), así que un crash
	// a media juntada re-arranca desde el cursor persistido: idempotente.
	var pending []PullChange
	for {
		resp, err := a.getFull(ctx, cursor)
		if err != nil {
			return err
		}
		pending = append(pending, resp.Changes...)
		batch := pending
		applyErr := ApplyPullPage(ctx, a.uow, batch, func(tx sharedDomain.Transaction) error {
			if resp.HasMore {
				return SetFullSyncCursor(ctx, tx, resp.NextCursor)
			}
			// Finalize: mark initial sync done, clear cursor, set
			// last_pulled_at to server_now so subsequent /pull is incremental.
			if err := ClearFullSyncCursor(ctx, tx); err != nil {
				return err
			}
			if err := SetInitialSyncCompletedAt(ctx, tx, resp.ServerNow); err != nil {
				return err
			}
			return SetLastPulledAt(ctx, tx, resp.ServerNow)
		})
		if applyErr != nil {
			if isFKConstraintErr(applyErr) && resp.HasMore {
				cursor = resp.NextCursor
				continue
			}
			if isFKConstraintErr(applyErr) {
				// Se acabó el feed y la FK sigue rota: dato huérfano en el
				// cloud. SQLite no dice qué fila — el diagnóstico sí.
				return localApplyErr(fmt.Errorf("%w — %s", applyErr, describeFKViolations(ctx, a.uow, batch)))
			}
			return localApplyErr(applyErr)
		}
		pending = nil
		if !resp.HasMore {
			a.mu.Lock()
			a.state.InitialSyncCompletedAt = resp.ServerNow
			a.state.LastPulledAt = resp.ServerNow
			a.mu.Unlock()
			return nil
		}
		cursor = resp.NextCursor
	}
}

func (a *Agent) loadFullCursor(ctx context.Context) (string, error) {
	tx, err := a.uow.Query(ctx)
	if err != nil {
		return "", err
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	var v sql.NullString
	err = stx.Get(ctx, &v, `SELECT value FROM sync_state WHERE key = ?`, keyFullSyncCursor)
	if err == sql.ErrNoRows || !v.Valid {
		return "", nil
	}
	return v.String, err
}

func (a *Agent) getFull(ctx context.Context, cursor string) (*FullSyncResponse, error) {
	endpoint, err := url.JoinPath(a.cfg.BaseURL, "/api/v1/sync/full")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	q.Set("limit", strconv.Itoa(a.cfg.FullSyncPageSize))
	endpoint += "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.currentToken())
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUpgradeRequired {
		return nil, ErrSchemaUpgradeRequired
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("full %d: %s", resp.StatusCode, body)
	}
	var fr FullSyncResponse
	if err := json.NewDecoder(resp.Body).Decode(&fr); err != nil {
		return nil, err
	}
	return &fr, nil
}

// ── State plumbing ──────────────────────────────────────────────────────

func (a *Agent) recordSuccess(ctx context.Context) {
	now := time.Now().UTC()
	a.mu.Lock()
	a.state.LastSyncedAt = now
	a.state.LastError = ""
	a.state.ConsecutiveFailures = 0
	a.state.NextRetryAt = time.Time{}
	// Un ciclo exitoso invalida cualquier 401 previo — si la credencial
	// estaba muerta no habría llegado hasta acá. Lo mismo aplica para el
	// flag de schema_upgrade_required: si pudimos sincronizar es que el
	// binario ya está al día.
	a.state.AuthInvalid = false
	a.state.WaitingForAuth = false
	a.state.SchemaUpgradeRequired = false
	a.state.LocalApplyError = false
	a.mu.Unlock()
	_ = a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		_ = SetLastSyncedAt(ctx, tx, now)
		_ = ClearLastError(ctx, tx)
		_ = SetConsecutiveFailures(ctx, tx, 0)
		return nil
	})
	a.refreshPendingCount(ctx)
}

func (a *Agent) recordFailure(ctx context.Context, phase string, err error) {
	a.cfg.Logger.Printf("[sync] %s failed: %v", phase, err)
	authInvalid := isAuthError(err)
	schemaStale := errors.Is(err, ErrSchemaUpgradeRequired)
	a.mu.Lock()
	a.state.ConsecutiveFailures++
	a.state.LastError = phase + ": " + err.Error()
	// Refleja la naturaleza del ÚLTIMO fallo: aplicación local vs red. Un
	// fallo de red posterior limpia el flag (el operador SÍ debe ver "Sin
	// internet" entonces).
	a.state.LocalApplyError = errors.Is(err, ErrLocalApply)
	wait := backoff(a.state.ConsecutiveFailures)
	if authInvalid {
		// La credencial está muerta hasta que el operador re-loguee. Los
		// retries automáticos no van a recuperar nada — pisamos el backoff
		// con un cooldown largo (1h) para no martillar al cloud, y marcamos
		// el estado para que la UI muestre "Vuelve a iniciar sesión" en vez
		// de "Sin internet".
		a.state.AuthInvalid = true
		wait = time.Hour
	}
	if schemaStale {
		// El cloud rechazó el schema_version del sidecar. Ningún retry sirve
		// hasta que el binario se actualice — pisamos el backoff con 1h igual
		// que con auth invalid, y marcamos el flag para que la UI muestre el
		// modal bloqueante "Tu versión ya no es compatible" (ADR-005 §2.7).
		a.state.SchemaUpgradeRequired = true
		wait = time.Hour
	}
	a.state.NextRetryAt = time.Now().Add(wait)
	failures := a.state.ConsecutiveFailures
	nextRetry := a.state.NextRetryAt
	lastErr := a.state.LastError
	a.mu.Unlock()
	_ = a.uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		_ = SetLastError(ctx, tx, lastErr)
		_ = SetConsecutiveFailures(ctx, tx, failures)
		_ = SetNextRetryAt(ctx, tx, nextRetry)
		return nil
	})
}

// backoff: 1s, 2s, 4s, 8s, 16s, 30s, 60s, capped at 5min (ADR-001 §3.3).
func backoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	steps := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 60 * time.Second,
	}
	if failures-1 < len(steps) {
		return steps[failures-1]
	}
	return 5 * time.Minute
}

func (a *Agent) markPushedAt(t time.Time) {
	a.mu.Lock()
	a.state.LastPushedAt = t
	a.mu.Unlock()
}

// stuckPushThreshold — cuántos rechazos per-item acumula una fila de la
// cola antes de considerarla ATORADA (y pintar sync_error). >1 para no
// alarmar por un blip transitorio: cada ciclo la reintenta.
const stuckPushThreshold = 3

// maxStuckItemsInStatus — cap del detalle de filas atoradas que viaja en
// /sync/status. Suficiente para cualquier caso real (los atorones vienen
// de a unas cuantas filas); el conteo StuckPushCount sí es total.
const maxStuckItemsInStatus = 20

func (a *Agent) refreshPendingCount(ctx context.Context) {
	tx, err := a.uow.Query(ctx)
	if err != nil {
		return
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	if err := stx.Get(ctx, &n, `SELECT COUNT(*) FROM sync_queue WHERE synced_at IS NULL`); err != nil {
		return
	}
	var stuck int
	_ = stx.Get(ctx, &stuck,
		`SELECT COUNT(*) FROM sync_queue WHERE synced_at IS NULL AND retry_count >= ?`,
		stuckPushThreshold)
	items := a.loadStuckItems(ctx, stx)
	// StuckPushError conserva su rol (muestra de la fila más castigada en
	// el detalle del indicador) pero ahora ya viene clasificado — sin el
	// prefijo "rejected_duplicate:" que sólo era jerga para el operador.
	var sample string
	for _, it := range items {
		if it.Message != "" {
			sample = it.Message
			break
		}
	}
	a.mu.Lock()
	a.state.PendingCount = n
	a.state.StuckPushCount = stuck
	a.state.StuckPushError = sample
	a.state.StuckItems = items
	a.mu.Unlock()
}

// loadStuckItems lee el detalle de las filas atoradas de sync_queue y las
// clasifica para el status. Best-effort: un error de lectura devuelve nil y
// el status degrada al comportamiento previo (sólo conteos).
func (a *Agent) loadStuckItems(ctx context.Context, stx *sharedDomain.SqlxTransaction) []StuckQueueItem {
	rows, err := stx.Ext().QueryxContext(ctx, `
		SELECT id, entity_type, entity_id, operation, retry_count,
		       COALESCE(last_error, ''), payload
		  FROM sync_queue
		 WHERE synced_at IS NULL AND retry_count >= ?
		 ORDER BY retry_count DESC, rowid ASC
		 LIMIT ?`,
		stuckPushThreshold, maxStuckItemsInStatus)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []StuckQueueItem
	for rows.Next() {
		var (
			id, entityType, entityID, op, lastErr, payload string
			retryCount                                     int
		)
		if err := rows.Scan(&id, &entityType, &entityID, &op, &retryCount, &lastErr, &payload); err != nil {
			return nil
		}
		kind, msg := classifyStuckError(lastErr)
		out = append(out, StuckQueueItem{
			QueueID:     id,
			EntityType:  entityType,
			EntityID:    entityID,
			Operation:   op,
			RetryCount:  retryCount,
			Kind:        kind,
			Message:     msg,
			EntityLabel: entityLabelFromPayload([]byte(payload)),
		})
	}
	return out
}

// classifyStuckError deriva (kind, mensaje limpio) del last_error persistido
// en sync_queue:
//
//   - "rejected_duplicate: <msg>" — cloud nuevo; el prefijo se quita y el
//     mensaje ya viene legible en español (server_duplicates.go).
//   - error crudo de Postgres con SQLSTATE 23505 — cloud viejo que aún no
//     mapea duplicados; se clasifica igual (el CTA del desktop funciona)
//     aunque el texto quede crudo.
//   - cualquier otra cosa — kind other, texto tal cual.
func classifyStuckError(lastError string) (kind, message string) {
	if rest, ok := strings.CutPrefix(lastError, StatusRejectedDuplicate+": "); ok {
		return StuckKindDuplicate, rest
	}
	if strings.Contains(lastError, "SQLSTATE 23505") ||
		strings.Contains(lastError, "duplicate key value violates unique constraint") {
		return StuckKindDuplicate, lastError
	}
	return StuckKindOther, lastError
}

// entityLabelFromPayload extrae un identificador humano del snapshot
// encolado para que la lista de atorados no muestre UUIDs. El orden de
// llaves cubre los tipos con más probabilidad de atorarse (catálogos con
// nombre, socios, pagos con folio, overrides de plantillas).
func entityLabelFromPayload(payload []byte) string {
	var pl map[string]any
	if err := json.Unmarshal(payload, &pl); err != nil {
		return ""
	}
	for _, k := range []string{"name", "full_name", "code", "folio", "template_key", "email"} {
		if s, ok := pl[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func (a *Agent) setStateLabel(label string) {
	a.mu.Lock()
	a.state.State = label
	a.mu.Unlock()
}

// Errors the agent translates uniformly. Wrapped so callers can check
// errors.Is(err, ErrSchemaUpgradeRequired) without string matching.
var (
	ErrSchemaUpgradeRequired = errors.New("schema upgrade required by server")
	ErrUnauthorized          = errors.New("sync auth rejected (token expired?)")
	// ErrLocalApply marca un fallo al APLICAR datos en el SQLite local
	// (pull/full-sync que aterriza, o el write-back de un push aceptado),
	// distinto de una falla de red/transporte. Es load-bearing para el
	// estado que ve el operador: un "no such column" o un CHECK que
	// rebota NO es "Sin internet" — el request llegó y el cloud respondió;
	// lo que falló es el guardado local. Sin esta distinción, un bug de
	// aplicación se disfrazaba de problema de conexión y el operador
	// perseguía el router en vez de reportar el error real.
	ErrLocalApply = errors.New("local apply failed")
)

// localApplyErr envuelve un error de aplicación local con ErrLocalApply,
// preservando la cadena original (Go 1.20+ multi-%w) para que
// isFKConstraintErr (errors.As sqlite3.Error) y los mensajes de
// diagnóstico sigan funcionando aguas abajo.
func localApplyErr(err error) error {
	return fmt.Errorf("%w: %w", ErrLocalApply, err)
}

// isAuthError returns true when err signals "el cloud rechazó nuestras
// credenciales", de manera que recordFailure pueda transicionar el agente
// a AuthInvalid en vez de seguir reintentando con el token muerto. Los
// uploads/downloads a R2 también arman fmt.Errorf con "401" embebido, por
// eso revisamos también el string.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUnauthorized) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized")
}
