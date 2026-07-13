//go:build server

package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// UpsertResult is the per-item outcome of Store.UpsertOne. It carries enough
// state to build the wire response (PushItemResult) and a ConflictLog entry
// when applicable, without the handler having to re-read the row.
type UpsertResult struct {
	Status                string
	ServerVersion         int
	ServerUpdatedAt       time.Time
	ServerPayload         json.RawMessage
	PreviousServerVersion int
	PreviousServerPayload json.RawMessage
	IsConflict            bool
	// Error: razón legible de un rechazo (viaja al sidecar y aterriza en
	// sync_queue.last_error → visible en "Estado de sincronización").
	Error string
}

// Store is the cloud persistence surface used by the sync handlers. The
// production implementation is PostgresStore; unit tests use InMemoryStore.
type Store interface {
	UpsertOne(ctx context.Context, tx sharedDomain.Transaction, gymID uuid.UUID, item PushItem) (UpsertResult, error)
	ListSince(ctx context.Context, tx sharedDomain.Transaction, gymID uuid.UUID, since time.Time, limit int) ([]PullChange, bool, error)
	ListForFullSync(ctx context.Context, tx sharedDomain.Transaction, gymID uuid.UUID, cursor FullCursor, limit int) ([]PullChange, FullCursor, bool, error)
}

// FullCursor encodes resumable position across topological order. We split
// it into (entity_type_index, server_updated_at, entity_id) so it survives
// new entries with the same updated_at (entity_id breaks ties).
type FullCursor struct {
	TypeIndex int       `json:"t"`
	After     time.Time `json:"a"`
	EntityID  string    `json:"e"`
}

// EncodeCursor — serializes a FullCursor to a URL-safe base64-ish opaque
// string so clients treat it as opaque (ADR-001 §3.5).
func EncodeCursor(c FullCursor) string {
	if c.TypeIndex == 0 && c.After.IsZero() && c.EntityID == "" {
		return ""
	}
	b, _ := json.Marshal(c)
	return string(b)
}

// DecodeCursor — parses an opaque cursor; returns zero value on empty input.
func DecodeCursor(s string) (FullCursor, error) {
	if s == "" {
		return FullCursor{}, nil
	}
	var c FullCursor
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return FullCursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	return c, nil
}

// PostgresStore implements Store against the cloud `sync_entities` table.
// All work runs inside a caller-provided GORM transaction (ADR-001 §3.3:
// serializable Postgres transaction per push item).
type PostgresStore struct {
	// MaxClockSkew caps how far the sidecar's payload.updated_at may sit
	// from the server's wall clock before we reject. Zero disables the
	// check (legacy behaviour). 5 minutes is generous enough for any
	// reasonable NTP drift but tight enough that a BIOS several hours off
	// can't win LWW against legitimate cloud writes.
	MaxClockSkew time.Duration
}

const defaultMaxClockSkew = 5 * time.Minute

func NewPostgresStore() *PostgresStore {
	return &PostgresStore{MaxClockSkew: defaultMaxClockSkew}
}

// TouchEntity bumpea server_updated_at de una fila EXISTENTE del journal para
// que el próximo pull incremental (ListSince) la incluya. Es el mecanismo
// para cambios cloud-authoritative que escriben la tabla canónica sin pasar
// por el push pipeline (p.ej. webhooks de Stripe/MP mutando la suscripción
// del gym): el valor vivo lo inyecta la augmentation al LEER, así que basta
// mover el timestamp — el payload guardado no se re-serializa.
//
// NO inserta si la fila no existe (un gym cuyo sidecar nunca ha pusheado no
// tiene fila; fabricar un payload parcial aquí arriesgaría pisar campos en el
// apply del sidecar). Devuelve touched=false en ese caso — el bootstrap /
// full sync cubre a esos gyms.
func (s *PostgresStore) TouchEntity(
	ctx context.Context,
	tx sharedDomain.Transaction,
	gymID uuid.UUID,
	entityType string,
	entityID uuid.UUID,
) (bool, error) {
	g := gormTx(tx)
	res := g.WithContext(ctx).Exec(`
		UPDATE sync_entities SET server_updated_at = NOW()
		 WHERE gym_id = ? AND entity_type = ? AND entity_id = ?`,
		gymID, entityType, entityID)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// TouchGym — re-mandar la fila del gym tras un cambio de suscripción.
// Satisface el puerto subscriptions/app.GymSyncToucher.
//
// A diferencia de TouchEntity, además de mover server_updated_at BUMPEA la
// versión del journal. El touch timestamp-only sólo propagaba a devices con
// versión atrasada: tras un push exitoso, el sidecar que pusheó queda con
// local.version == journal.version (stampLocalSynced), y su ApplyPullChange
// descarta la fila touched por LWW (local.version >= change.version) — en el
// caso común de UN solo desktop, el cambio de suscripción nunca aterrizaba.
//
// version = GREATEST(se.version + 1, gy.version):
//   - `se.version + 1` garantiza bump ESTRICTO sobre lo que el último push
//     dejó stampeado en el sidecar. No basta igualar gyms.version: tras un
//     conflict client-wins el journal queda ADELANTE del row vivo (updateRow
//     escribe row.Version+1 pero el projector proyecta a gyms la versión del
//     payload del cliente), y un GREATEST plano sería no-op de versión.
//   - `gy.version` porque los writes cloud-side bumpean gyms.version vía
//     dominio (p.ej. Gym.ActivateSubscription en RecordEvent) en la misma tx
//     ANTES del touch — en steady state gy = se+1 y el journal cae exacto en
//     la versión del row vivo.
//
// Trade-off deliberado: un push offline encolado con la versión empatada cae
// al conflict path (LWW por updated_at) en vez de aceptarse limpio. Es la
// semántica documentada, y el client-wins es seguro para billing: enqueueGym
// no emite subscription_* y el projector sólo escribe llaves presentes.
func (s *PostgresStore) TouchGym(ctx context.Context, tx sharedDomain.Transaction, gymID uuid.UUID) error {
	g := gormTx(tx)
	return g.WithContext(ctx).Exec(`
		UPDATE sync_entities se
		   SET server_updated_at = NOW(),
		       version = GREATEST(se.version + 1, gy.version)
		  FROM gyms gy
		 WHERE gy.id = se.entity_id
		   AND se.gym_id = ? AND se.entity_type = 'gyms' AND se.entity_id = ?`,
		gymID, gymID).Error
}

// UpsertOne applies one push item with LWW semantics. Returns an
// UpsertResult describing what happened (accepted / conflict_*) so the
// handler can build both the wire response and the conflict-log entry.
func (s *PostgresStore) UpsertOne(
	ctx context.Context,
	tx sharedDomain.Transaction,
	gymID uuid.UUID,
	item PushItem,
) (UpsertResult, error) {
	g := gormTx(tx)
	entityID, err := uuid.Parse(item.EntityID)
	if err != nil {
		return UpsertResult{}, fmt.Errorf("invalid entity_id: %w", err)
	}

	// Validate payload.gym_id matches JWT gym_id (defense in depth — the
	// handler also checks, but we re-verify here so the Store is safe to
	// call from any caller).
	var pl map[string]any
	if err := json.Unmarshal(item.Payload, &pl); err != nil {
		return UpsertResult{}, fmt.Errorf("payload not a JSON object: %w", err)
	}
	if v, ok := pl["gym_id"].(string); !ok || v != gymID.String() {
		return UpsertResult{Status: StatusRejectedUnauthorized}, nil
	}

	// Clock skew check — SÓLO hacia el FUTURO. El LWW usa payload.updated_at
	// como timestamp autoritativo del cliente; un reloj ADELANTADO (BIOS
	// desincronizado, batería CMOS muerta) ganaría todos los conflictos
	// contra cambios cloud legítimos — eso sí se rechaza, permanente, y que
	// el operador alinee NTP (clamp-to-now corrompería el orden de
	// operaciones del propio sidecar).
	//
	// Un updated_at en el PASADO es lo contrario de un problema: es la
	// operación offline normal. La versión anterior rechazaba por |drift|,
	// así que cualquier cambio hecho sin conexión (o con el cloud caído)
	// por más de 5 minutos quedaba imposible de subir PARA SIEMPRE — la
	// fila se atoraba en sync_queue con reintentos infinitos, rompiendo la
	// promesa central del producto ("la operación diaria nunca depende de
	// internet"). Un payload viejo no gana nada injustamente: el LWW lo
	// resuelve solo.
	if s.MaxClockSkew > 0 {
		if ct := extractUpdatedAt(pl); !ct.IsZero() {
			if drift := ct.Sub(time.Now().UTC()); drift > s.MaxClockSkew {
				return UpsertResult{
					Status: StatusRejectedClockSkew,
					// Con razón legible: antes viajaba sin mensaje y el
					// sidecar guardaba last_error vacío — el indicador
					// decía "Último error: Ninguno" con filas atoradas,
					// indiagnosticable en campo.
					Error: fmt.Sprintf(
						"updated_at del payload está %s en el futuro (máximo %s): revisa el reloj de la computadora",
						drift.Round(time.Second), s.MaxClockSkew),
				}, nil
			}
		}
	}

	// Lock the row (or absence) for the duration of this transaction.
	row, exists, err := s.lockRow(ctx, g, gymID, item.EntityType, entityID)
	if err != nil {
		return UpsertResult{}, err
	}

	now := time.Now().UTC()

	// Determine whether this is a delete operation by either operation flag
	// or the payload's deleted_at field (the sidecar always sends a snapshot
	// so the source of truth is the payload).
	isDelete := item.Operation == OpDeleteStr
	var deletedAt *time.Time
	if isDelete {
		deletedAt = &now
	} else if dv, ok := pl["deleted_at"]; ok && dv != nil {
		// Payload has deleted_at — accept it and treat as soft-delete.
		// Server still uses NOW() as canonical timestamp on first sync.
		deletedAt = &now
	}

	if !exists {
		// First sight — accept unconditionally.
		if err := s.insertRow(ctx, g, gymID, item.EntityType, entityID, item.ClientVersion, item.Payload, now, deletedAt); err != nil {
			return UpsertResult{}, err
		}
		if err := project(g, item.EntityType, gymID, entityID, item.Payload); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{
			Status:          StatusAccepted,
			ServerVersion:   item.ClientVersion,
			ServerUpdatedAt: now,
		}, nil
	}

	// Compare versions for LWW (ADR-001 §3.3).
	if row.Version < item.ClientVersion {
		// Client is newer — accept.
		if err := s.updateRow(ctx, g, gymID, item.EntityType, entityID, item.ClientVersion, item.Payload, now, deletedAt); err != nil {
			return UpsertResult{}, err
		}
		if err := project(g, item.EntityType, gymID, entityID, item.Payload); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{
			Status:          StatusAccepted,
			ServerVersion:   item.ClientVersion,
			ServerUpdatedAt: now,
		}, nil
	}

	// Conflict path. Resolve by client_now vs server.updated_at proxy: the
	// sidecar's payload.updated_at (epoch ms) compared to row.ServerUpdatedAt.
	// If client's local updated_at is strictly after server's, client wins.
	clientLocalUpdatedAt := extractUpdatedAt(pl)
	if !clientLocalUpdatedAt.IsZero() && clientLocalUpdatedAt.After(row.ServerUpdatedAt) {
		newVersion := row.Version + 1
		if item.ClientVersion > newVersion {
			newVersion = item.ClientVersion + 1
		}
		if err := s.updateRow(ctx, g, gymID, item.EntityType, entityID, newVersion, item.Payload, now, deletedAt); err != nil {
			return UpsertResult{}, err
		}
		if err := project(g, item.EntityType, gymID, entityID, item.Payload); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{
			Status:                StatusConflictClientWins,
			ServerVersion:         newVersion,
			ServerUpdatedAt:       now,
			IsConflict:            true,
			PreviousServerVersion: row.Version,
			PreviousServerPayload: row.Payload,
		}, nil
	}
	// Server wins — return canonical state without writing.
	return UpsertResult{
		Status:                StatusConflictServerWins,
		ServerVersion:         row.Version,
		ServerUpdatedAt:       row.ServerUpdatedAt,
		ServerPayload:         row.Payload,
		IsConflict:            true,
		PreviousServerVersion: row.Version,
		PreviousServerPayload: row.Payload,
	}, nil
}

type syncEntityRow struct {
	Version         int
	Payload         json.RawMessage
	ServerUpdatedAt time.Time
	DeletedAt       *time.Time
}

// lockRow acquires SELECT FOR UPDATE on the sync_entities row (ADR-001
// §3.3). exists=false when the row is missing — caller will INSERT.
func (s *PostgresStore) lockRow(
	ctx context.Context,
	g *gorm.DB,
	gymID uuid.UUID,
	entityType string,
	entityID uuid.UUID,
) (syncEntityRow, bool, error) {
	const q = `
		SELECT version, payload, server_updated_at, deleted_at
		FROM sync_entities
		WHERE gym_id = ? AND entity_type = ? AND entity_id = ?
		FOR UPDATE`
	var rec struct {
		Version         int
		Payload         []byte
		ServerUpdatedAt time.Time `gorm:"column:server_updated_at"`
		DeletedAt       *time.Time
	}
	res := g.WithContext(ctx).Raw(q, gymID, entityType, entityID).Scan(&rec)
	if res.Error != nil {
		return syncEntityRow{}, false, res.Error
	}
	if res.RowsAffected == 0 {
		return syncEntityRow{}, false, nil
	}
	return syncEntityRow{
		Version:         rec.Version,
		Payload:         rec.Payload,
		ServerUpdatedAt: rec.ServerUpdatedAt,
		DeletedAt:       rec.DeletedAt,
	}, true, nil
}

func (s *PostgresStore) insertRow(
	ctx context.Context,
	g *gorm.DB,
	gymID uuid.UUID,
	entityType string,
	entityID uuid.UUID,
	version int,
	payload json.RawMessage,
	now time.Time,
	deletedAt *time.Time,
) error {
	const q = `
		INSERT INTO sync_entities
		    (gym_id, entity_type, entity_id, version, payload, server_updated_at, deleted_at)
		VALUES (?, ?, ?, ?, ?::jsonb, ?, ?)`
	return g.WithContext(ctx).Exec(q, gymID, entityType, entityID, version, string(payload), now, deletedAt).Error
}

func (s *PostgresStore) updateRow(
	ctx context.Context,
	g *gorm.DB,
	gymID uuid.UUID,
	entityType string,
	entityID uuid.UUID,
	version int,
	payload json.RawMessage,
	now time.Time,
	deletedAt *time.Time,
) error {
	const q = `
		UPDATE sync_entities
		   SET version = ?, payload = ?::jsonb, server_updated_at = ?, deleted_at = ?
		 WHERE gym_id = ? AND entity_type = ? AND entity_id = ?`
	return g.WithContext(ctx).Exec(q, version, string(payload), now, deletedAt, gymID, entityType, entityID).Error
}

// gymCanonicalAugmentExpr — para sync_entities.entity_type='gyms',
// inyecta cuatro grupos de columnas desde el row vivo de `gyms`:
//
//  1. Billing (cloud-owned): subscription_plan, subscription_status,
//     stripe_customer_id, trial_ends_at, subscription_ends_at. El sidecar
//     nunca emite estos en su push (CLAUDE.md: cloud es source of truth).
//     Sin la inyección, un sidecar full-syncing rompe NOT NULL al INSERT
//     porque subscription_plan + subscription_status son NOT NULL en SQLite.
//
//  2. created_at: immutable; el row del cloud lo tiene siempre poblado
//     (DEFAULT NOW() en Postgres). Sidecars antiguos pre-fix no lo
//     emitían en su enqueueGym, dejando payloads stored con la llave
//     ausente — la INSERT en SQLite (que sí tiene NOT NULL sin default)
//     fallaba. Inyectar acá hace innecesaria cualquier backfill manual.
//
//  3. kiosk_settings: sidecar-owned, pero proyectado al gym row vivo
//     en cada push. Leerlo de gyms en lugar del payload garantiza que
//     pull-eo siempre recibe el último valor reconocido por cloud,
//     incluso si el payload guardado es viejo.
//
//  4. WhatsApp Business (cloud-owned, UC-037): whatsapp_business_phone,
//     whatsapp_connected_at, whatsapp_business_token_enc. El ceremony de
//     connect/disconnect vive sólo en el cloud (necesita provider real +
//     internet; el desktop lo alcanza vía el WhatsAppSidecarProxy) y
//     enqueueGym los omite del push — esta inyección es el ÚNICO camino
//     por el que el estado de conexión llega a los sidecars. token_enc es
//     BYTEA: viaja como base64 (mismo wire que el JSON de Go para []byte)
//     para que blobColumns en agent_apply.go lo decodifique; translate()
//     quita los saltos de línea que encode(...,'base64') mete cada 76
//     chars (RFC 2045), que base64.StdEncoding no tolera. Hoy es NULL by
//     design (MVP guarda senderID en audit, no credenciales) y el NULL
//     sobrevive el encode.
//
// El operador `||` de jsonb concatena objetos con prioridad al lado
// derecho — entonces el valor del cloud SIEMPRE gana sobre el payload.
// Para 1/4 (billing, whatsapp) es la regla; para 2/3 es no-op semántico
// (gym row = proyección del último push) y arregla cualquier payload
// pre-fix sin necesidad de tocar sync_entities.
//
// Timestamps TIMESTAMPTZ se serializan como epoch ms (bigint) — formato
// wire del sidecar. NULLs sobreviven: jsonb_build_object emite JSON null
// donde corresponda (ej. gym recién creado sin trial_ends_at).
//
// Para filas non-gym (members, payments, ...) o cuando el JOIN no
// encuentra el gym (defensa: gym fue hard-deleted o existe sólo en
// sync_entities), el CASE devuelve el payload original sin tocar.
const gymCanonicalAugmentExpr = `
	CASE
	    WHEN se.entity_type = 'gyms' AND g.id IS NOT NULL THEN
	        se.payload || jsonb_build_object(
	            'subscription_plan',    g.subscription_plan,
	            'subscription_status',  g.subscription_status,
	            'stripe_customer_id',   g.stripe_customer_id,
	            'trial_ends_at',
	                CASE WHEN g.trial_ends_at IS NULL THEN NULL
	                ELSE (EXTRACT(EPOCH FROM g.trial_ends_at) * 1000)::bigint END,
	            'subscription_ends_at',
	                CASE WHEN g.subscription_ends_at IS NULL THEN NULL
	                ELSE (EXTRACT(EPOCH FROM g.subscription_ends_at) * 1000)::bigint END,
	            'created_at',     (EXTRACT(EPOCH FROM g.created_at) * 1000)::bigint,
	            'kiosk_settings', g.kiosk_settings,
	            'whatsapp_business_phone', g.whatsapp_business_phone,
	            'whatsapp_connected_at',
	                CASE WHEN g.whatsapp_connected_at IS NULL THEN NULL
	                ELSE (EXTRACT(EPOCH FROM g.whatsapp_connected_at) * 1000)::bigint END,
	            'whatsapp_business_token_enc',
	                translate(encode(g.whatsapp_business_token_enc, 'base64'), E'\n', '')
	        )
	    ELSE se.payload
	END`

// ListSince — incremental pull (GET /sync/pull). Returns up to `limit` rows
// with `server_updated_at > since`, ordered by (server_updated_at,
// entity_id) for deterministic resume. hasMore=true means the client
// should issue another pull with the last server_updated_at it saw.
func (s *PostgresStore) ListSince(
	ctx context.Context,
	tx sharedDomain.Transaction,
	gymID uuid.UUID,
	since time.Time,
	limit int,
) ([]PullChange, bool, error) {
	g := gormTx(tx)
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	q := `
		SELECT se.entity_type, se.entity_id, se.version,
		       ` + gymCanonicalAugmentExpr + ` AS payload,
		       se.server_updated_at, se.deleted_at
		  FROM sync_entities se
		  LEFT JOIN gyms g ON se.entity_type = 'gyms' AND se.entity_id = g.id
		 WHERE se.gym_id = ? AND se.server_updated_at > ?
		 ORDER BY se.server_updated_at ASC, se.entity_id ASC
		 LIMIT ?`
	rows, err := g.WithContext(ctx).Raw(q, gymID, since, limit+1).Rows()
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]PullChange, 0, limit)
	for rows.Next() {
		var (
			entityType   string
			entityID     uuid.UUID
			version      int
			payloadBytes []byte
			updatedAt    time.Time
			deletedAt    *time.Time
		)
		if err := rows.Scan(&entityType, &entityID, &version, &payloadBytes, &updatedAt, &deletedAt); err != nil {
			return nil, false, err
		}
		out = append(out, PullChange{
			EntityType:      entityType,
			EntityID:        entityID.String(),
			Version:         version,
			Payload:         json.RawMessage(payloadBytes),
			ServerUpdatedAt: updatedAt,
			DeletedAt:       deletedAt,
		})
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// ListForFullSync — paginates the full set in topological order (one
// entity_type at a time). The cursor advances within a type until
// exhausted, then moves to the next type. This makes recovery resumable
// from any point (ADR-001 §3.5 step 5).
func (s *PostgresStore) ListForFullSync(
	ctx context.Context,
	tx sharedDomain.Transaction,
	gymID uuid.UUID,
	cursor FullCursor,
	limit int,
) ([]PullChange, FullCursor, bool, error) {
	g := gormTx(tx)
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}

	out := make([]PullChange, 0, limit)
	currentCursor := cursor

	for currentCursor.TypeIndex < len(SyncedTables) {
		entityType := SyncedTables[currentCursor.TypeIndex].Type
		remaining := limit - len(out)
		if remaining <= 0 {
			break
		}
		// Page within this entity_type. Misma augmentación cloud-canonical
		// que ListSince — sin ella un sidecar full-syncing rompe NOT NULL
		// al INSERT (created_at, subscription_plan, subscription_status,
		// kiosk_settings son todos NOT NULL en SQLite).
		q := `
			SELECT se.entity_type, se.entity_id, se.version,
			       ` + gymCanonicalAugmentExpr + ` AS payload,
			       se.server_updated_at, se.deleted_at
			  FROM sync_entities se
			  LEFT JOIN gyms g ON se.entity_type = 'gyms' AND se.entity_id = g.id
			 WHERE se.gym_id = ?
			   AND se.entity_type = ?
			   AND (
			        se.server_updated_at > ?
			     OR (se.server_updated_at = ? AND se.entity_id > ?)
			   )
			 ORDER BY se.server_updated_at ASC, se.entity_id ASC
			 LIMIT ?`
		// Postgres rejects "" bound to a uuid column with SQLSTATE 22P02. The
		// cursor's EntityID is empty on the very first call (no previous row
		// to break the timestamp tie against) — substitute uuid.Nil so the
		// `entity_id > ?` branch matches every real id.
		entityIDArg := currentCursor.EntityID
		if entityIDArg == "" {
			entityIDArg = uuid.Nil.String()
		}
		rows, err := g.WithContext(ctx).Raw(q,
			gymID, entityType,
			currentCursor.After, currentCursor.After, entityIDArg,
			remaining+1,
		).Rows()
		if err != nil {
			return nil, FullCursor{}, false, err
		}
		batch := make([]PullChange, 0, remaining)
		for rows.Next() {
			var (
				etype     string
				eid       uuid.UUID
				version   int
				payloadB  []byte
				updatedAt time.Time
				deletedAt *time.Time
			)
			if err := rows.Scan(&etype, &eid, &version, &payloadB, &updatedAt, &deletedAt); err != nil {
				rows.Close()
				return nil, FullCursor{}, false, err
			}
			batch = append(batch, PullChange{
				EntityType:      etype,
				EntityID:        eid.String(),
				Version:         version,
				Payload:         json.RawMessage(payloadB),
				ServerUpdatedAt: updatedAt,
				DeletedAt:       deletedAt,
			})
		}
		rows.Close()
		moreInType := len(batch) > remaining
		if moreInType {
			batch = batch[:remaining]
		}
		out = append(out, batch...)
		if moreInType {
			last := batch[len(batch)-1]
			currentCursor = FullCursor{
				TypeIndex: currentCursor.TypeIndex,
				After:     last.ServerUpdatedAt,
				EntityID:  last.EntityID,
			}
			break
		}
		// Type exhausted — advance.
		currentCursor = FullCursor{TypeIndex: currentCursor.TypeIndex + 1}
	}
	hasMore := currentCursor.TypeIndex < len(SyncedTables)
	if !hasMore {
		currentCursor = FullCursor{TypeIndex: len(SyncedTables)}
	}
	return out, currentCursor, hasMore, nil
}

// gormTx — pulls the underlying *gorm.DB from a sharedDomain.Transaction.
func gormTx(tx sharedDomain.Transaction) *gorm.DB {
	return tx.(*sharedDomain.GormTransaction).Tx
}

// extractUpdatedAt pulls the client-local updated_at out of a payload map.
// Accepts either an epoch-ms number (sidecar's existing format) or an ISO
// string. Returns zero time when missing or unparseable — the caller treats
// that as "no client-side info, assume server wins".
func extractUpdatedAt(pl map[string]any) time.Time {
	v, ok := pl["updated_at"]
	if !ok {
		return time.Time{}
	}
	switch x := v.(type) {
	case float64:
		// Epoch ms (existing sidecar format).
		return time.UnixMilli(int64(x)).UTC()
	case string:
		// Try a few formats — sidecar may evolve to ISO.
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t.UTC()
		}
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// ConflictLogger captures sync conflicts. The production implementation is
// PostgresConflictLogger (writes to the cloud `conflict_log` table); tests
// inject an in-memory recorder. The handler depends on the interface so
// it can run without a real DB.
type ConflictLogger interface {
	Log(ctx context.Context, tx sharedDomain.Transaction, e ConflictLogEntry) error
}

// ConflictLogEntry is what the handler hands to ConflictLogger.Log.
type ConflictLogEntry struct {
	GymID         uuid.UUID
	EntityType    string
	EntityID      uuid.UUID
	ClientID      uuid.UUID
	ClientVersion int
	ServerVersion int
	ClientPayload json.RawMessage
	ServerPayload json.RawMessage
	Resolution    string // 'server_wins' | 'client_wins'
}

// PostgresConflictLogger is the production impl: writes to conflict_log
// inside the caller's transaction, so the conflict row commits/rolls back
// atomically with the upsert that produced it.
type PostgresConflictLogger struct{}

// NewConflictLogger returns the production logger. Kept as the package's
// default name for back-compat with main.go wiring.
func NewConflictLogger() ConflictLogger { return &PostgresConflictLogger{} }

func (l *PostgresConflictLogger) Log(ctx context.Context, tx sharedDomain.Transaction, e ConflictLogEntry) error {
	g := gormTx(tx)
	const q = `
		INSERT INTO conflict_log
			(gym_id, entity_type, entity_id, client_id, client_version,
			 server_version, client_payload, server_payload, resolution, detected_at)
		VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, ?::jsonb, ?, NOW())`
	clientPayload := normalizeJSON(e.ClientPayload)
	serverPayload := normalizeJSON(e.ServerPayload)
	return g.WithContext(ctx).Exec(q,
		e.GymID, e.EntityType, e.EntityID, e.ClientID, e.ClientVersion,
		e.ServerVersion, clientPayload, serverPayload, e.Resolution,
	).Error
}

func normalizeJSON(b json.RawMessage) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "{}"
	}
	return s
}

// ErrSchemaUpgrade is returned by handlers when the request schema_version
// is greater than the server's. Maps to HTTP 426 Upgrade Required.
var ErrSchemaUpgrade = errors.New("schema upgrade required")
