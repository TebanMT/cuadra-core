//go:build server

package sync

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/sidecartoken"
)

// Handler wires the cloud-side /sync endpoints. It is intentionally small —
// all heavy lifting lives in Store and ConflictLogger so this file stays a
// thin HTTP adapter.
type Handler struct {
	UoW          sharedDomain.UnitOfWork
	Store        Store
	Conflicts    ConflictLogger
	Tokens       auth.TokenService
	SidecarStore sidecartoken.Store
	Metrics      *Metrics
}

func NewHandler(uow sharedDomain.UnitOfWork, store Store, conflicts ConflictLogger, tokens auth.TokenService, sidecarStore sidecartoken.Store, metrics *Metrics) *Handler {
	if metrics == nil {
		metrics = NewMetrics()
	}
	return &Handler{UoW: uow, Store: store, Conflicts: conflicts, Tokens: tokens, SidecarStore: sidecarStore, Metrics: metrics}
}

// RegisterRoutes mounts the four cloud sync endpoints + the metrics
// endpoint at /_internal/metrics (ADR-001 §5). The auth gate accepts both
// sk_live_* sidecar credentials and operator JWTs (ADR-008 §3.2) so the
// migration window keeps working sidecars on either path.
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1/sync")
	if h.SidecarStore != nil {
		api.Use(middleware.SidecarOrJWTMiddleware(h.Tokens, h.SidecarStore))
	} else {
		api.Use(middleware.AuthMiddleware(h.Tokens))
	}
	api.POST("/push", h.Push)
	api.GET("/pull", h.Pull)
	api.GET("/full", h.FullSync)
	api.GET("/last-update", h.LastUpdate)
	r.GET("/_internal/metrics", h.MetricsHandler)
}

// LastUpdate handles GET /api/v1/sync/last-update. Devuelve la última vez
// que un sidecar del gym tocó el cloud (push, pull, o cualquier request) —
// el dashboard lo pinta como "tu equipo lleva X sin sincronizar".
//
// Fuente: sidecar_credentials.last_seen_at, que el middleware actualiza en
// CADA request del sidecar (no sólo en push). Antes leíamos
// sync_entities.MAX(server_updated_at), pero ese timestamp sólo se mueve
// con pushes — un sidecar conectado SIN actividad nueva (sin altas, sin
// pagos) parecía "stale" aunque estuviera vivo y sincronizando vía pull.
//
// Cuando un gym tiene varios sidecars (multi-device), tomamos el más
// reciente: si AL MENOS un equipo está conectado, el banner se silencia.
func (h *Handler) LastUpdate(c *gin.Context) {
	gymID, ok := middleware.GetGymID(c)
	if !ok {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if h.SidecarStore == nil {
		// Cluster sin sidecar store wired — devolver null en lugar de 500
		// para que el banner se silencie en lugar de loguear errores.
		c.JSON(http.StatusOK, gin.H{"last_synced_at": nil})
		return
	}
	creds, err := h.SidecarStore.ListActiveByGym(c.Request.Context(), gymID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var lastAt *time.Time
	for i := range creds {
		t := creds[i].LastSeenAt
		if t.IsZero() {
			continue
		}
		if lastAt == nil || t.After(*lastAt) {
			tt := t
			lastAt = &tt
		}
	}
	// Shape exacta que el dashboard espera (useSyncStatus.ts:CloudSyncStatus).
	// last_synced_at = null cuando el gym no tiene sidecar pareado todavía.
	c.JSON(http.StatusOK, gin.H{"last_synced_at": lastAt})
}

// Push handles POST /api/v1/sync/push. Each item is processed in its own
// transaction so a single bad item doesn't roll back the whole batch
// (ADR-001 §3.3 — serializable per item).
func (h *Handler) Push(c *gin.Context) {
	start := time.Now()
	defer func() { h.Metrics.ObserveDuration("push", time.Since(start)) }()

	gymID, ok := middleware.GetGymID(c)
	if !ok {
		h.Metrics.IncPushRequest("rejected")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	var req PushRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.Metrics.IncPushRequest("malformed")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	if req.SchemaVersion > SchemaVersion {
		h.Metrics.IncPushRequest("schema_upgrade")
		h.Metrics.IncSchemaUpgrade()
		c.AbortWithStatusJSON(http.StatusUpgradeRequired, gin.H{
			"error":          "schema_upgrade_required",
			"server_version": SchemaVersion,
			"client_version": req.SchemaVersion,
		})
		return
	}
	if req.SchemaVersion <= 0 {
		req.SchemaVersion = 1
	}

	clientUUID := uuid.Nil
	if cid, err := uuid.Parse(req.ClientID); err == nil {
		clientUUID = cid
	}

	results := make([]PushItemResult, 0, len(req.Batch))
	ctx := c.Request.Context()
	now := time.Now().UTC()

	for _, item := range req.Batch {
		res := h.processOne(ctx, gymID, clientUUID, item)
		h.Metrics.IncPushItem(res.Status)
		results = append(results, res)
	}

	h.Metrics.IncPushRequest("ok")
	c.JSON(http.StatusOK, PushResponse{
		ServerNow:     now,
		SchemaVersion: SchemaVersion,
		Results:       results,
	})
}

func (h *Handler) processOne(ctx context.Context, gymID, clientID uuid.UUID, item PushItem) PushItemResult {
	if item.QueueID == "" || item.EntityID == "" || item.EntityType == "" {
		return PushItemResult{
			QueueID:  item.QueueID,
			EntityID: item.EntityID,
			Status:   StatusRejectedInternal,
			Error:    "missing required field (queue_id, entity_id, entity_type)",
		}
	}
	if FindTable(item.EntityType) == nil {
		return PushItemResult{
			QueueID:  item.QueueID,
			EntityID: item.EntityID,
			Status:   StatusRejectedUnknownType,
			Error:    "unknown entity_type: " + item.EntityType,
		}
	}
	if item.Operation != OpUpsertStr && item.Operation != OpDeleteStr {
		return PushItemResult{
			QueueID:  item.QueueID,
			EntityID: item.EntityID,
			Status:   StatusRejectedInternal,
			Error:    "invalid operation: " + item.Operation,
		}
	}

	var out PushItemResult
	out.QueueID = item.QueueID
	out.EntityID = item.EntityID

	err := h.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		ur, err := h.Store.UpsertOne(ctx, tx, gymID, item)
		if err != nil {
			return err
		}
		// Apply LWW & build wire response.
		out.Status = ur.Status
		out.ServerVersion = ur.ServerVersion
		if !ur.ServerUpdatedAt.IsZero() {
			t := ur.ServerUpdatedAt
			out.ServerUpdatedAt = &t
		}
		if ur.Status == StatusConflictServerWins && len(ur.ServerPayload) > 0 {
			out.ServerPayload = ur.ServerPayload
		}

		// Conflict logging (ADR-001 §3.7) — best-effort but inside the same
		// tx so it commits/rolls back atomically with the upsert.
		if ur.IsConflict {
			entityID, _ := uuid.Parse(item.EntityID)
			resolution := "server_wins"
			if ur.Status == StatusConflictClientWins {
				resolution = "client_wins"
			}
			h.Metrics.IncConflict(resolution)
			err := h.Conflicts.Log(ctx, tx, ConflictLogEntry{
				GymID:         gymID,
				EntityType:    item.EntityType,
				EntityID:      entityID,
				ClientID:      clientID,
				ClientVersion: item.ClientVersion,
				ServerVersion: ur.PreviousServerVersion,
				ClientPayload: item.Payload,
				ServerPayload: ur.PreviousServerPayload,
				Resolution:    resolution,
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Distinguish auth-style rejections (returned from UpsertOne with
		// status=rejected_unauthorized and nil err) from real DB failures.
		out.Status = StatusRejectedInternal
		out.Error = err.Error()
	}
	return out
}

// Pull handles GET /api/v1/sync/pull?since=...&limit=...
func (h *Handler) Pull(c *gin.Context) {
	start := time.Now()
	defer func() { h.Metrics.ObserveDuration("pull", time.Since(start)) }()

	gymID, ok := middleware.GetGymID(c)
	if !ok {
		h.Metrics.IncPullRequest("rejected")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	since := parseTime(c.Query("since"))
	limit := 500
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	var changes []PullChange
	var hasMore bool
	err := h.UoW.Command(c.Request.Context(), func(tx sharedDomain.Transaction) error {
		var err error
		changes, hasMore, err = h.Store.ListSince(c.Request.Context(), tx, gymID, since, limit)
		return err
	})
	if err != nil {
		h.Metrics.IncPullRequest("error")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.Metrics.IncPullRequest("ok")
	h.Metrics.IncPullItems(len(changes))

	resp := PullResponse{
		ServerNow:     time.Now().UTC(),
		SchemaVersion: SchemaVersion,
		Changes:       changes,
		HasMore:       hasMore,
	}
	c.JSON(http.StatusOK, resp)
}

// FullSync handles GET /api/v1/sync/full?cursor=...&limit=...
func (h *Handler) FullSync(c *gin.Context) {
	start := time.Now()
	defer func() { h.Metrics.ObserveDuration("full", time.Since(start)) }()

	gymID, ok := middleware.GetGymID(c)
	if !ok {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	rawCursor := c.Query("cursor")
	cursor, err := DecodeCursor(rawCursor)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	limit := 1000
	if l := c.Query("limit"); l != "" {
		if n, perr := strconv.Atoi(l); perr == nil && n > 0 {
			limit = n
		}
	}

	var changes []PullChange
	var nextCursor FullCursor
	var hasMore bool
	cerr := h.UoW.Command(c.Request.Context(), func(tx sharedDomain.Transaction) error {
		var e error
		changes, nextCursor, hasMore, e = h.Store.ListForFullSync(c.Request.Context(), tx, gymID, cursor, limit)
		return e
	})
	if cerr != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": cerr.Error()})
		return
	}

	h.Metrics.IncFullRequest()
	h.Metrics.IncPullItems(len(changes))

	resp := FullSyncResponse{
		ServerNow:     time.Now().UTC(),
		SchemaVersion: SchemaVersion,
		Changes:       changes,
		HasMore:       hasMore,
	}
	if hasMore {
		resp.NextCursor = EncodeCursor(nextCursor)
	}
	c.JSON(http.StatusOK, resp)
}

// MetricsHandler exposes Prometheus text format at /_internal/metrics.
// It deliberately has no auth — this is meant to be scraped from a
// trusted Prom exporter on the same host (see ADR-001 §4.5).
func (h *Handler) MetricsHandler(c *gin.Context) {
	c.Data(http.StatusOK, "text/plain; version=0.0.4", []byte(h.Metrics.Render()))
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	// Accept epoch ms numeric form.
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(n).UTC()
	}
	return time.Time{}
}
