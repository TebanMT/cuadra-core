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
)
