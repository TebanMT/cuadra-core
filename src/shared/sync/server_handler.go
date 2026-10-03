//go:build server

package sync

import (
	"context"
	"errors"
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
	// SMK (Server Master Key) habilita el escrow de GMKs (ADR-006 §2.2,
	// POST /sync/gmk). Se setea con WithSMK desde cmd/server; vacía =
	// endpoint responde 503 y los sidecars conservan su llave local.
	SMK []byte
}

// WithSMK habilita el escrow de GMK. Encadenable tras NewHandler.
func (h *Handler) WithSMK(smk []byte) *Handler {
	h.SMK = smk
	return h
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
	// Escrow de GMK (ADR-006) — misma frontera de confianza que push/pull;
	// requiere WithSMK, sin ella responde 503 (ver server_gmk.go).
	api.POST("/gmk", h.EnsureGMK)
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

// Push handles POST /api/v1/sync/push. Ordinary items keep their historical
// per-item transactions. Correlated refund graphs are the deliberate
// exception: their mutable Payment snapshots, immutable Refund and line items
// are one financial command and therefore commit or roll back together.
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

	if !h.checkReceiptSchema(c, gymID, req.SchemaVersion) {
		return
	}
	clientUUID := uuid.Nil
	if cid, err := uuid.Parse(req.ClientID); err == nil {
		clientUUID = cid
	}

	results := make([]PushItemResult, len(req.Batch))
	ctx := c.Request.Context()
	now := time.Now().UTC()

	expenseGroups := make(map[int][]int)
	if _, ok := h.Store.(*PostgresStore); ok {
		for _, indexes := range groupExpensePushItems(req.Batch) {
			for _, index := range indexes {
				expenseGroups[index] = indexes
			}
		}
	}
	handledExpenses := make(map[int]bool)
	for _, unit := range groupRefundPushItems(req.Batch) {
		if handledExpenses[unit.indexes[0]] {
			continue
		}
		if indexes, ok := expenseGroups[unit.indexes[0]]; ok {
			for index, result := range h.processExpenseGraph(ctx, gymID, clientUUID, req.Batch, indexes, req.SchemaVersion) {
				results[index] = result
				handledExpenses[index] = true
			}
			continue
		}
		if len(unit.indexes) == 1 && len(unit.graphIDs) == 0 {
			index := unit.indexes[0]
			results[index] = h.processOne(ctx, gymID, clientUUID, req.Batch[index])
		} else {
			unitResults := h.processRefundGraph(ctx, gymID, clientUUID, req.Batch, unit)
			for index, result := range unitResults {
				results[index] = result
			}
		}
	}
	for _, result := range results {
		h.Metrics.IncPushItem(result.Status)
	}

	h.Metrics.IncPushRequest("ok")
	c.JSON(http.StatusOK, PushResponse{
		ServerNow:     now,
		SchemaVersion: SchemaVersion,
		Results:       results,
	})
}

func (h *Handler) processOne(ctx context.Context, gymID, clientID uuid.UUID, item PushItem) PushItemResult {
	if invalid := validatePushItem(item); invalid != nil {
		return *invalid
	}

	out := PushItemResult{QueueID: item.QueueID, EntityID: item.EntityID}

	err := h.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		return h.applyOneInTx(ctx, tx, gymID, clientID, item, &out)
	})
	if err != nil {
		classifyPushError(&out, err, item)
	}
	return out
}

func validatePushItem(item PushItem) *PushItemResult {
	if item.QueueID == "" || item.EntityID == "" || item.EntityType == "" {
		return &PushItemResult{QueueID: item.QueueID, EntityID: item.EntityID,
			Status: StatusRejectedInternal, Error: "missing required field (queue_id, entity_id, entity_type)"}
	}
	if FindTable(item.EntityType) == nil {
		return &PushItemResult{QueueID: item.QueueID, EntityID: item.EntityID,
			Status: StatusRejectedUnknownType, Error: "unknown entity_type: " + item.EntityType}
	}
	if item.Operation != OpUpsertStr && item.Operation != OpDeleteStr {
		return &PushItemResult{QueueID: item.QueueID, EntityID: item.EntityID,
			Status: StatusRejectedInternal, Error: "invalid operation: " + item.Operation}
	}
	return nil
}

func (h *Handler) applyOneInTx(
	ctx context.Context,
	tx sharedDomain.Transaction,
	gymID, clientID uuid.UUID,
	item PushItem,
	out *PushItemResult,
) error {
	ur, err := h.Store.UpsertOne(ctx, tx, gymID, item)
	if err != nil {
		return err
	}
	out.Status = ur.Status
	out.Error = ur.Error
	out.ServerVersion = ur.ServerVersion
	if !ur.ServerUpdatedAt.IsZero() {
		t := ur.ServerUpdatedAt
		out.ServerUpdatedAt = &t
	}
	if ur.Status == StatusConflictServerWins && len(ur.ServerPayload) > 0 {
		out.ServerPayload = ur.ServerPayload
	}

	// Conflict logging (ADR-001 §3.7) remains inside the caller's transaction;
	// a graph rollback must remove its diagnostic rows too.
	if ur.IsConflict {
		entityID, _ := uuid.Parse(item.EntityID)
		resolution := "server_wins"
		if ur.Status == StatusConflictClientWins {
			resolution = "client_wins"
		}
		h.Metrics.IncConflict(resolution)
		if err := h.Conflicts.Log(ctx, tx, ConflictLogEntry{
			GymID: gymID, EntityType: item.EntityType, EntityID: entityID, ClientID: clientID,
			ClientVersion: item.ClientVersion, ServerVersion: ur.PreviousServerVersion,
			ClientPayload: item.Payload, ServerPayload: ur.PreviousServerPayload, Resolution: resolution,
		}); err != nil {
			return err
		}
	}
	return nil
}

func classifyPushError(out *PushItemResult, err error, item PushItem) {
	var financialConflict *financialConflictError
	if errors.As(err, &financialConflict) {
		out.Status = StatusRejectedFinancialConflict
		out.Error = financialConflict.Error()
	} else if msg, isDup := mapUniqueViolation(err, item); isDup {
		out.Status = StatusRejectedDuplicate
		out.Error = msg
	} else {
		out.Status = StatusRejectedInternal
		out.Error = err.Error()
	}
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

	version, _ := strconv.Atoi(c.GetHeader("X-Tinta-Sync-Schema"))
	if !h.checkReceiptSchema(c, gymID, version) {
		return
	}
	since := parseTime(c.Query("since"))
	pullCursor, decodeErr := DecodeCursor(c.Query("cursor"))
	if decodeErr != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "cursor inválido"})
		return
	}
	if pullCursor.EntityID != "" {
		if _, err := uuid.Parse(pullCursor.EntityID); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "cursor inválido"})
			return
		}
	}
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
		if store, ok := h.Store.(interface {
			ListSinceCursor(context.Context, sharedDomain.Transaction, uuid.UUID, FullCursor, int) ([]PullChange, bool, error)
		}); ok && c.Query("cursor") != "" {
			changes, hasMore, err = store.ListSinceCursor(c.Request.Context(), tx, gymID, pullCursor, limit)
		} else {
			changes, hasMore, err = h.Store.ListSince(c.Request.Context(), tx, gymID, since, limit)
		}
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
	if len(changes) > 0 {
		last := changes[len(changes)-1]
		resp.NextCursor = EncodeCursor(FullCursor{After: last.ServerUpdatedAt, EntityID: last.EntityID, EntityType: last.EntityType})
	}
	if !h.checkReceiptSchema(c, gymID, version) {
		return
	} // cover a feature activation concurrent with the read
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

	version, _ := strconv.Atoi(c.GetHeader("X-Tinta-Sync-Schema"))
	if !h.checkReceiptSchema(c, gymID, version) {
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
		ServerNow:     start.UTC(),
		SchemaVersion: SchemaVersion,
		Changes:       changes,
		HasMore:       hasMore,
	}
	if hasMore {
		resp.NextCursor = EncodeCursor(nextCursor)
	}
	if !h.checkReceiptSchema(c, gymID, version) {
		return
	} // cover a feature activation concurrent with the read
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

// Gate reads too: an old desktop must not partially apply a nullable purchase
// link and silently skip its new receipt type.
func (h *Handler) checkReceiptSchema(c *gin.Context, gymID uuid.UUID, version int) bool {
	if version < 6 {
		if gate, ok := h.Store.(interface {
			RequiresProductCreditSchema(context.Context, sharedDomain.Transaction, uuid.UUID) (bool, error)
		}); ok {
			var required bool
			err := sharedDomain.ReadSnapshot(c.Request.Context(), h.UoW, func(tx sharedDomain.Transaction) error {
				var err error
				required, err = gate.RequiresProductCreditSchema(c.Request.Context(), tx, gymID)
				return err
			})
			if err != nil {
				c.AbortWithStatusJSON(500, gin.H{"error": "no se pudo verificar la compatibilidad de sincronización"})
				return false
			}
			if required {
				c.AbortWithStatusJSON(http.StatusUpgradeRequired, gin.H{"error": "schema_upgrade_required", "server_version": SchemaVersion, "minimum_version": 6, "message": "actualiza Tinta en recepción para sincronizar las ventas fiadas"})
				return false
			}
		}
	}

	if version < 5 {
		if gate, ok := h.Store.(interface {
			RequiresPurchaseRegistrationSchema(context.Context, sharedDomain.Transaction, uuid.UUID) (bool, error)
		}); ok {
			var required bool
			err := sharedDomain.ReadSnapshot(c.Request.Context(), h.UoW, func(tx sharedDomain.Transaction) error {
				var e error
				required, e = gate.RequiresPurchaseRegistrationSchema(c.Request.Context(), tx, gymID)
				return e
			})
			if err != nil {
				c.AbortWithStatusJSON(500, gin.H{"error": "no se pudo verificar la compatibilidad de sincronización"})
				return false
			}
			if required {
				c.AbortWithStatusJSON(http.StatusUpgradeRequired, gin.H{"error": "schema_upgrade_required", "server_version": SchemaVersion, "minimum_version": 5, "message": "actualiza Tinta en recepción para sincronizar las compras y existencias"})
				return false
			}
		}
	}

	if version >= 3 {
		return true
	}
	gate, ok := h.Store.(interface {
		RequiresReceiptSchema(context.Context, sharedDomain.Transaction, uuid.UUID) (bool, error)
	})
	if !ok {
		return true
	}
	var required bool
	err := sharedDomain.ReadSnapshot(c.Request.Context(), h.UoW, func(tx sharedDomain.Transaction) error {
		var err error
		required, err = gate.RequiresReceiptSchema(c.Request.Context(), tx, gymID)
		return err
	})
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "no se pudo verificar la compatibilidad de sincronización"})
		return false
	}
	if required {
		c.AbortWithStatusJSON(http.StatusUpgradeRequired, gin.H{"error": "schema_upgrade_required", "server_version": SchemaVersion, "minimum_version": 3, "message": "actualiza Tinta en recepción para sincronizar las compras registradas en la web"})
		return false
	}
	return true
}
