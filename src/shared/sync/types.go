package sync

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the wire-protocol version. Servers reject pushes whose
// `schema_version` is greater than this (ADR-001 §3.8) with 426 Upgrade
// Required. Bump when the wire shape changes incompatibly.
const SchemaVersion = 1

// Operation values match the sync_queue.operation CHECK constraint
// (ADR-001 §3.2).
const (
	OpUpsertStr = "upsert"
	OpDeleteStr = "delete"
)

// Result statuses returned by /sync/push for each item in the batch
// (ADR-001 §3.3). The client maps these to local actions (mark synced,
// rebase, alert, retry).
const (
	StatusAccepted             = "accepted"
	StatusConflictServerWins   = "conflict_server_wins"
	StatusConflictClientWins   = "conflict_client_wins"
	StatusRejectedUnauthorized = "rejected_unauthorized"
	StatusRejectedSchema       = "rejected_schema_version"
	StatusRejectedUnknownType  = "rejected_unknown_entity_type"
	StatusRejectedInternal     = "rejected_internal_error"
	// StatusRejectedClockSkew — el payload.updated_at del sidecar está
	// fuera del rango aceptable comparado contra el reloj del servidor
	// (default ±5 min, configurable vía MaxClockSkew en PostgresStore).
	// El sidecar lo trata como permanent: el operador debe sincronizar su
	// reloj con NTP. Sin esta validación, un BIOS adelantado 6h hace que
	// el cliente gane TODOS los conflictos contra cambios cloud legítimos.
	StatusRejectedClockSkew = "rejected_clock_skew"
	// StatusRejectedDuplicate — la proyección chocó con un unique index de
	// la nube (SQLSTATE 23505): otro device (o el propio cloud) ya ocupa el
	// valor — p.ej. dos recepciones crean el plan "Mensual" cada una.
	// Permanente por diseño: reintentar el MISMO payload jamás va a entrar;
	// lo que destraba es que el operador EDITE el registro local (renombrar
	// re-encola el snapshot nuevo por coalescing y el siguiente push pasa).
	// Error viaja ya legible en español (ver server_duplicates.go) — el
	// texto aterriza en sync_queue.last_error y de ahí al indicador del
	// desktop con CTA de resolución.
	StatusRejectedDuplicate = "rejected_duplicate"
)

// PushItem is one entry of a push batch. Payload is kept as raw JSON so the
// receiver can hand it to the Applier without re-encoding, and so unknown
// fields survive forward-compat scenarios (ADR-001 §3.8).
type PushItem struct {
	QueueID       string          `json:"queue_id"`
	EntityType    string          `json:"entity_type"`
	EntityID      string          `json:"entity_id"`
	Operation     string          `json:"operation"`
	ClientVersion int             `json:"client_version"`
	Payload       json.RawMessage `json:"payload"`
	EnqueuedAt    time.Time       `json:"enqueued_at"`
}

// PushRequest is the full body of POST /api/v1/sync/push.
type PushRequest struct {
	ClientID      string     `json:"client_id"`
	ClientNow     time.Time  `json:"client_now"`
	SchemaVersion int        `json:"schema_version"`
	Batch         []PushItem `json:"batch"`
}

// PushItemResult mirrors PushItem 1:1 in the response.
type PushItemResult struct {
	QueueID         string          `json:"queue_id"`
	EntityID        string          `json:"entity_id"`
	Status          string          `json:"status"`
	ServerVersion   int             `json:"server_version,omitempty"`
	ServerUpdatedAt *time.Time      `json:"server_updated_at,omitempty"`
	ServerPayload   json.RawMessage `json:"server_payload,omitempty"`
	Error           string          `json:"error,omitempty"`
}

// PushResponse is the body returned by POST /api/v1/sync/push.
type PushResponse struct {
	ServerNow     time.Time        `json:"server_now"`
	SchemaVersion int              `json:"schema_version"`
	Results       []PushItemResult `json:"results"`
}

// PullChange is one entity returned by GET /api/v1/sync/pull or /full.
type PullChange struct {
	EntityType      string          `json:"entity_type"`
	EntityID        string          `json:"entity_id"`
	Version         int             `json:"version"`
	Payload         json.RawMessage `json:"payload"`
	ServerUpdatedAt time.Time       `json:"server_updated_at"`
	DeletedAt       *time.Time      `json:"deleted_at,omitempty"`
}

// PullResponse — body of GET /api/v1/sync/pull.
type PullResponse struct {
	ServerNow     time.Time    `json:"server_now"`
	SchemaVersion int          `json:"schema_version"`
	Changes       []PullChange `json:"changes"`
	NextCursor    string       `json:"next_cursor,omitempty"`
	HasMore       bool         `json:"has_more"`
}

// FullSyncResponse — body of GET /api/v1/sync/full. Identical to PullResponse
// in shape; semantics differ (no `since` filter, ordered topologically).
type FullSyncResponse = PullResponse

// StatusResponse — body of GET /api/v1/sync/status (sidecar local).
type StatusResponse struct {
	State               string     `json:"state"`
	LastSyncedAt        *time.Time `json:"last_synced_at,omitempty"`
	LastPulledAt        *time.Time `json:"last_pulled_at,omitempty"`
	QueuePendingCount   int        `json:"queue_pending_count"`
	LastError           string     `json:"last_error,omitempty"`
	InitialSyncDone     bool       `json:"initial_sync_completed"`
	NextRetryAt         *time.Time `json:"next_retry_at,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	// AuthInvalid — el cloud rechazó la credencial. El FE renderea CTA de
	// re-login en lugar de "Sin internet". También true cuando WaitingForAuth
	// (sidecar nunca recibió token, ej. recién canjeado pero el desktop
	// aún no llamó al login) — desde la perspectiva del operador es el
	// mismo problema: hace falta autenticar.
	AuthInvalid bool `json:"auth_invalid,omitempty"`
	// SchemaUpgradeRequired — el cloud devolvió 426 (ADR-001 §3.8). El
	// FE muestra un modal bloqueante "Tu versión ya no es compatible.
	// Actualízala." con CTA que dispara update inmediato (ADR-005 §2.7).
	// Mayor prioridad que cualquier offline_*: aunque haya internet, el
	// cliente está roto hasta que actualice.
	SchemaUpgradeRequired bool `json:"schema_upgrade_required,omitempty"`
	// LocalApplyError — el último fallo fue al aplicar datos localmente,
	// no de red. El FE lo usa para mostrar un mensaje accionable en lugar
	// de "Sin internet" (ver StateSyncError).
	LocalApplyError bool `json:"local_apply_error,omitempty"`
	// QuarantinedCount — filas que el pull saltó tras fallar el umbral de
	// intentos. >0 mantiene el estado en StateSyncError aunque el resto
	// del sync fluya, para que saltar cambios NUNCA sea silencioso.
	QuarantinedCount int `json:"quarantined_count,omitempty"`
	// QueueStuckCount — filas de subida que el server rechazó per-item
	// varias veces (el ciclo global "exitoso" las dejaba invisibles: el
	// indicador decía "Sincronizado" con la cola pudriéndose). >0 también
	// mantiene StateSyncError.
	QueueStuckCount int `json:"queue_stuck_count,omitempty"`
	// QueueStuckItems — detalle de las filas atoradas (cap a
	// maxStuckItemsInStatus, ordenadas por castigo). Antes el status sólo
	// traía el conteo + UN last_error de muestra; el operador veía "2
	// cambios rechazados" sin saber CUÁLES ni cómo destrabarlos. Con el
	// detalle, el desktop puede ofrecer acción por fila (p.ej. un rechazo
	// kind=duplicate sobre membership_types abre el plan para renombrarlo).
	QueueStuckItems []StuckQueueItem `json:"queue_stuck_items,omitempty"`
}

// Kind de un StuckQueueItem — clasificación gruesa del rechazo para que el
// FE decida qué acción ofrecer sin parsear mensajes.
const (
	// StuckKindDuplicate: unique violation en la nube. Permanente hasta que
	// el operador edite el registro local — renombrar re-encola (coalescing
	// de sync_queue) y el siguiente push entra limpio.
	StuckKindDuplicate = "duplicate"
	// StuckKindOther: cualquier otro rechazo persistente (FK huérfana,
	// schema, etc.) — visible pero sin acción directa de UI.
	StuckKindOther = "other"
)

// StuckQueueItem — una fila de sync_queue que el server rechazó per-item
// stuckPushThreshold+ veces. Viaja dentro de /sync/status (sidecar local)
// para que el desktop muestre QUÉ está atorado, POR QUÉ (Message ya
// legible cuando el cloud es reciente) y ofrezca resolución.
type StuckQueueItem struct {
	QueueID    string `json:"queue_id"`
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	Operation  string `json:"operation"`
	RetryCount int    `json:"retry_count"`
	// Kind — StuckKindDuplicate | StuckKindOther.
	Kind string `json:"kind"`
	// Message — razón del rechazo, sin el prefijo de status. Para clouds
	// viejos que aún mandan el error crudo de Postgres, viaja tal cual
	// (la clasificación por SQLSTATE sigue funcionando).
	Message string `json:"message"`
	// EntityLabel — identificador humano extraído del payload encolado
	// ("Mensual", "Agua Ciel 1L", folio…) para que la lista del desktop
	// no muestre UUIDs.
	EntityLabel string `json:"entity_label,omitempty"`
}

// State values returned by /sync/status, per UC-044.
const (
	StateOnline          = "online"
	StateOfflineShort    = "offline_short"    // <5 min
	StateOfflineMedium   = "offline_medium"   // 5 min – 24 h
	StateOfflineLong     = "offline_long"     // 24 h – 7 d
	StateOfflineCritical = "offline_critical" // >7 d
	StateInitialSyncing  = "initial_syncing"  // full-sync in progress
	// StateAuthInvalid — el cloud rechazó la credencial del sidecar (401).
	// Distinto de offline_*: ahí internet funciona, falta re-autenticar. La
	// UI muestra un CTA explícito ("vuelve a iniciar sesión") en lugar de
	// la genérica "Sin internet" que sólo confundiría.
	StateAuthInvalid = "auth_invalid"
	// StateSchemaUpgradeRequired — el cloud devolvió 426 (ADR-001 §3.8).
	// El binario quedó atrás; ningún retry recupera. UI dispara modal
	// bloqueante "Tu versión ya no es compatible".
	StateSchemaUpgradeRequired = "schema_upgrade_required"
	// StateSyncError — el último fallo fue al APLICAR datos localmente
	// (ErrLocalApply), no de red: el request llegó y el cloud respondió,
	// pero el guardado local rebotó (bug de esquema, CHECK, dato huérfano).
	// La UI lo muestra accionable ("Hay un problema al guardar cambios —
	// actualiza o reporta"), NUNCA "Sin internet": el operador que persigue
	// el router no lo va a arreglar. Prioridad debajo de auth/schema (otros
	// bloqueos) y encima de la clasificación offline.
	StateSyncError = "sync_error"
)
