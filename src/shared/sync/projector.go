//go:build server

package sync

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Projector materialises a single sync payload into the domain table that
// owns it. Runs inside the same serializable transaction as the
// sync_entities upsert (ADR-008 §3.1) so a partial application is impossible.
type Projector func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error

// jsonbColumns lists the columns per table that Postgres expects as JSONB.
// The generic projector wraps these in `?::jsonb` casts because the wire
// payload carries them as JSON values (objects/arrays) that round-trip via
// re-marshal.
var jsonbColumns = map[string]map[string]bool{
	"gyms":               {"payment_methods": true, "kiosk_settings": true, "charge_settings": true},
	"notification_queue": {"payload": true},
	"audit_log":          {"changes": true},
	// payments.breakdown carries the per-concept line items as a JSON
	// array (BreakdownLine[]). Without this entry pgx serialises the
	// Go slice as a Postgres record/tuple and the upsert fails with
	// "expression is of type record" (SQLSTATE 42804).
	"payments": {"breakdown": true},
	// subscription_events.raw_payload — el cuerpo del webhook normalizado
	// como objeto JSON. Mismo tratamiento que cualquier otra columna JSONB.
	"subscription_events": {"raw_payload": true},
}

// byteaColumns lists BYTEA columns that arrive in the payload base64-encoded.
// The projector base64-decodes them so Postgres receives raw bytes.
var byteaColumns = map[string]map[string]bool{
	"member_fingerprints": {"template_encrypted": true},
	"gyms":                {"whatsapp_business_token_enc": true},
}

// payloadKeyAliases maps payload field names to the actual table column. The
// sidecar emits `deleted_at_ms` for member_fingerprints (epoch ms) instead of
// `deleted_at` — projector translates the alias on the way in. New aliases
// belong here, not scattered across each projector.
var payloadKeyAliases = map[string]map[string]string{
	"member_fingerprints": {"deleted_at_ms": "deleted_at"},
}

// timestamptzColumns lists the extra columns whose Postgres type is
// TIMESTAMPTZ but whose name does NOT end in `_at`, so the suffix
// heuristic in isTimestampColumn would miss them. Without an entry here,
// epoch-ms ints from the sidecar payload reach pgx as raw float64 and the
// upsert dies with "cannot find encode plan for OID 1184".
//
// Discover new entries by grepping `db_migrations/postgres/` for
// `TIMESTAMPTZ` and filtering out names ending in `_at`. Mirror to SQLite
// is irrelevant — sidecar stores epoch-ms regardless of column name.
var timestamptzColumns = map[string]map[string]bool{
	"challenges":         {"measurement_t0_deadline": true, "measurement_t1_start": true},
	"notification_queue": {"scheduled_for": true},
	// subscription_events tiene tres TIMESTAMPTZ que no terminan en `_at`-
	// del-record-estándar: occurred_at sí matchea, pero period_ends_at va
	// con sufijo distinto y recorded_at es semántica BE — los tres llegan
	// como epoch-ms del sidecar (en realidad sólo cloud → sidecar pull,
	// pero declararlos hace simétrico el wire shape si algún día cambia).
	"subscription_events": {"period_ends_at": true, "occurred_at": true, "recorded_at": true},
}

// notNullStringColumns lists columns declared NOT NULL in Postgres whose
// dominio semántico trata "" como ausencia válida (ej. users.email tras la
// migración 019: el operador PIN-only no lleva email; "" = sin correo).
// Sin esta lista, nullifyEmptyString colapsa "" → NULL y Postgres rechaza
// con `null value in column "email" violates not-null constraint` (23502).
//
// Discover new entries with:
//
//	grep -E 'TEXT NOT NULL|VARCHAR\([0-9]+\) NOT NULL' db_migrations/postgres/*.sql
//
// y verificar contra el enqueue helper de cada repo SQLite: si la columna se
// envía como `""` cuando es opcional en el dominio, debe vivir aquí.
var notNullStringColumns = map[string]map[string]bool{
	"users": {"email": true},
}

// projectors is the dispatch table. Every entry in SyncedTables must have a
// projector registered or push fails with `missing_projector` (ADR-008 §3.1
// — fail loud, do not degrade silently). Today every projector delegates to
// the same generic implementation; the map exists so future entity-specific
// behaviour (e.g. denormalisation, side effects) can replace one entry
// without touching the rest.
var projectors = func() map[string]Projector {
	m := make(map[string]Projector, len(SyncedTables))
	var membershipsTable EntityTable
	for i := range SyncedTables {
		t := SyncedTables[i]
		if t.Type == "memberships" {
			membershipsTable = t
		}
		m[t.Type] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
			return projectGeneric(g, t, gymID, entityID, payload)
		}
	}
	// memberships necesita un pre-step: el índice parcial único
	// `uq_memberships_member_active` permite UNA sola fila por (gym,member)
	// con status active|pending_payment. El sidecar lo respeta con el
	// 3-step dance (services.go RenewMembershipForPayment), pero al cruzar
	// la frontera de sync cada item se proyecta en su propia transacción.
	// Si el cloud quedó con dos rows activas para el mismo socio (renewal
	// que se aplicó parcial, push fuera de orden por reintentos, o ids
	// generados por dos fuentes), la inserción del nuevo activo choca con
	// 23505. La resolución consistente con el dominio es "last write
	// wins el slot": demote el otro a 'replaced' antes de proyectar.
	m["memberships"] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
		return projectMembership(g, membershipsTable, gymID, entityID, payload)
	}
	return m
}()

// projectMembership envuelve projectGeneric con un pre-step que libera el
// slot del partial unique index `uq_memberships_member_active` cuando la
// fila entrante reclama el slot (status active|pending_payment, no
// soft-deleted). Cualquier otra fila vigente del mismo socio se baja a
// 'replaced' — misma semántica que la renovación del dominio.
func projectMembership(
	g *gorm.DB,
	table EntityTable,
	gymID, entityID uuid.UUID,
	payload []byte,
) error {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return fmt.Errorf("projector memberships: payload not a JSON object: %w", err)
	}
	status, _ := raw["status"].(string)
	claimsSlot := status == "active" || status == "pending_payment"
	// deleted_at nil/ausente significa "fila viva". Si viene set, la
	// proyección la marca borrada — no toca el slot de nadie más.
	deleted := false
	if v, ok := raw["deleted_at"]; ok && v != nil {
		// Cualquier valor no-nulo (string ISO, número epoch) indica delete.
		if s, isStr := v.(string); !isStr || s != "" {
			deleted = true
		}
	}
	memberIDStr, _ := raw["member_id"].(string)
	if claimsSlot && !deleted && memberIDStr != "" {
		memberID, err := uuid.Parse(memberIDStr)
		if err == nil {
			if err := vacateActiveMembershipSlot(g, gymID, memberID, entityID); err != nil {
				return fmt.Errorf("memberships pre-vacate active slot: %w", err)
			}
		}
	}
	return projectGeneric(g, table, gymID, entityID, payload)
}

// vacateActiveMembershipSlot baja a 'replaced' cualquier fila de memberships
// que esté actualmente ocupando el slot del partial unique index para
// (gymID, memberID) — excepto la que está por ser proyectada (skipID).
// Espeja la operación en sync_entities (payload + version + server_updated_at)
// para que el próximo pull del sidecar refleje el demote y mantenga
// coherente el server reloj (ADR-001 §3.1). Si no había conflicto, los
// UPDATE no afectan filas y el upsert principal sigue sin cambio.
func vacateActiveMembershipSlot(
	g *gorm.DB,
	gymID, memberID, skipID uuid.UUID,
) error {
	type demoted struct {
		ID      uuid.UUID
		Version int
	}
	var rows []demoted
	// Demote + RETURNING para conocer las filas afectadas y su nueva
	// version (la usamos para sync_entities). updated_at se reformulea
	// con NOW() de cloud — server reloj manda.
	const demoteSQL = `
		UPDATE memberships
		   SET status = 'replaced',
		       updated_at = NOW(),
		       version = version + 1
		 WHERE gym_id = ?
		   AND member_id = ?
		   AND id <> ?
		   AND status IN ('active','pending_payment')
		   AND deleted_at IS NULL
		RETURNING id, version`
	if err := g.Raw(demoteSQL, gymID, memberID, skipID).Scan(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	// Para cada fila demote, sincronizar sync_entities: status, version,
	// updated_at en payload + version + server_updated_at en la columna.
	// El payload usa epoch_ms (mismo formato que el sidecar emite) para
	// que ApplyPullChange en el sidecar no se confunda con el tipo.
	const syncSQL = `
		UPDATE sync_entities
		   SET payload = payload || jsonb_build_object(
		                     'status', 'replaced',
		                     'version', ?::int,
		                     'updated_at', (EXTRACT(EPOCH FROM NOW()) * 1000)::bigint
		                 ),
		       version = ?,
		       server_updated_at = NOW()
		 WHERE gym_id = ?
		   AND entity_type = 'memberships'
		   AND entity_id = ?`
	for _, r := range rows {
		if err := g.Exec(syncSQL, r.Version, r.Version, gymID, r.ID).Error; err != nil {
			return fmt.Errorf("sync_entities demote mirror for %s: %w", r.ID, err)
		}
	}
	return nil
}

// project dispatches to the registered projector for entityType. Returns a
// distinguished error when no projector is wired so the push handler can
// surface it as 500 missing_projector instead of pretending success.
func project(g *gorm.DB, entityType string, gymID, entityID uuid.UUID, payload []byte) error {
	p, ok := projectors[entityType]
	if !ok {
		return fmt.Errorf("missing_projector: no projector registered for entity_type=%q", entityType)
	}
	return p(g, gymID, entityID, payload)
}

// Project is the public wrapper around project() for one-off backfill
// scripts that need to replay sync_entities rows into the domain tables.
// Production push uses the unexported version inline.
func Project(g *gorm.DB, entityType string, gymID, entityID uuid.UUID, payload []byte) error {
	return project(g, entityType, gymID, entityID, payload)
}

// projectGeneric writes the payload into table.Table via INSERT … ON
// CONFLICT (id) DO UPDATE SET … using the column list declared in
// SyncedTables. Only columns present in the payload are emitted, which
// keeps schema defaults intact for new rows and preserves existing values
// for unmentioned columns on conflict (EXCLUDED is symmetric with the
// INSERT column list).
//
// Per-column type adjustments:
//   - JSONB columns get `?::jsonb` and the value is re-marshalled to JSON.
//   - BYTEA columns get base64-decoded into []byte.
//   - TIMESTAMPTZ columns receiving epoch-ms numbers get converted to
//     time.Time (Postgres rejects raw floats for timestamptz).
//   - Everything else rides pgx's native conversions.
func projectGeneric(
	g *gorm.DB,
	table EntityTable,
	gymID, entityID uuid.UUID,
	payload []byte,
) error {
	q, args, err := buildProjectorUpsert(table, gymID, entityID, payload)
	if err != nil {
		return err
	}
	return g.Exec(q, args...).Error
}

// buildProjectorUpsert produces the SQL + args for projectGeneric without
// executing it. Split out so unit tests can verify SQL shape without a
// running Postgres, and so the executor stays a one-liner.
func buildProjectorUpsert(
	table EntityTable,
	gymID, entityID uuid.UUID,
	payload []byte,
) (string, []any, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return "", nil, fmt.Errorf("projector %s: payload not a JSON object: %w", table.Type, err)
	}

	// Apply payload-key aliases (e.g. deleted_at_ms → deleted_at).
	if aliases := payloadKeyAliases[table.Table]; len(aliases) > 0 {
		for src, dst := range aliases {
			if v, ok := raw[src]; ok {
				raw[dst] = v
				delete(raw, src)
			}
		}
	}

	// Force id and gym_id from the upsert context (defense in depth — the
	// caller already validated payload.gym_id matches the JWT, but the
	// projector is the last writer so we re-pin both to avoid any future
	// bypass). Composite-key tables have no `id` column, so the assignment
	// is a no-op for them: the loop below only emits columns that appear in
	// table.Columns.
	raw["id"] = entityID.String()
	raw["gym_id"] = gymID.String()

	jsonCols := jsonbColumns[table.Table]
	byteCols := byteaColumns[table.Table]

	cols := make([]string, 0, len(table.Columns))
	placeholders := make([]string, 0, len(table.Columns))
	args := make([]any, 0, len(table.Columns))

	for _, c := range table.Columns {
		v, present := raw[c]
		if !present {
			continue
		}
		var (
			ph  = "?"
			arg any
		)
		switch {
		case jsonCols[c]:
			converted, err := encodeJSONB(v)
			if err != nil {
				return "", nil, fmt.Errorf("projector %s column %s: %w", table.Type, c, err)
			}
			arg = converted
			if converted != nil {
				ph = "?::jsonb"
			}
		case byteCols[c]:
			b, err := decodeBytea(v)
			if err != nil {
				return "", nil, fmt.Errorf("projector %s column %s: %w", table.Type, c, err)
			}
			arg = b
		case isTimestampColumn(table.Table, c):
			arg = coerceTimestamp(v)
		case notNullStringColumns[table.Table][c]:
			// NOT NULL column whose dominio acepta "" como ausencia válida —
			// preservamos el string vacío en lugar de colapsarlo a NULL (que
			// la constraint rechazaría). Ver notNullStringColumns arriba.
			arg = v
		default:
			arg = nullifyEmptyString(v)
		}
		cols = append(cols, c)
		placeholders = append(placeholders, ph)
		args = append(args, arg)
	}

	if len(cols) == 0 {
		return "", nil, fmt.Errorf("projector %s: empty payload", table.Type)
	}

	conflictCols := []string{"id"}
	if len(table.CompositeKey) > 0 {
		conflictCols = table.CompositeKey
	}
	conflictSet := make(map[string]struct{}, len(conflictCols))
	for _, c := range conflictCols {
		conflictSet[c] = struct{}{}
	}

	setClauses := make([]string, 0, len(cols))
	for _, c := range cols {
		// Don't update the conflict-key columns themselves on update —
		// they're the dedupe identity.
		if _, isKey := conflictSet[c]; isKey {
			continue
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = EXCLUDED.%s", c, c))
	}

	q := fmt.Sprintf(
		`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s`,
		table.Table,
		strings.Join(cols, ", "),
		strings.Join(placeholders, ", "),
		strings.Join(conflictCols, ", "),
		strings.Join(setClauses, ", "),
	)
	return q, args, nil
}

// encodeJSONB normalises any-typed payload values into the JSON text that
// pgx will hand to a `?::jsonb` cast. nil round-trips as SQL NULL; pre-
// stringified JSON passes through; anything else is re-marshalled.
func encodeJSONB(v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return x, nil
	case json.RawMessage:
		return string(x), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("jsonb marshal: %w", err)
		}
		return string(b), nil
	}
}

// decodeBytea unwraps a base64 payload value into raw bytes for BYTEA
// columns. The sidecar always sends BYTEA fields base64-encoded
// (sync_queue.payload is TEXT and JSON cannot carry binary).
func decodeBytea(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if x == "" {
			return nil, nil
		}
		return base64.StdEncoding.DecodeString(x)
	default:
		return nil, fmt.Errorf("bytea expected string, got %T", v)
	}
}

// isTimestampColumn returns true for columns whose Postgres type is
// timestamptz and whose payload value may arrive as an epoch-ms number.
//
// La mayoría de columnas timestamp del schema terminan en `_at`, así que
// esa es la heurística rápida. Para las que no (medición t0/t1 de retos,
// scheduled_for de notification_queue), el override por tabla en
// timestamptzColumns las atrapa. Las columnas tipo `_date` / `birthdate`
// llegan como strings ISO y Postgres las parsea sin coerción.
func isTimestampColumn(table, name string) bool {
	if strings.HasSuffix(name, "_at") {
		return true
	}
	if cols := timestamptzColumns[table]; cols != nil {
		return cols[name]
	}
	return false
}

// nullifyEmptyString collapses sidecar's `""` defaults into SQL NULL.
//
// The sidecar's enqueue helpers default optional `*string` fields to `""`
// when nil, which round-trips through JSON as the empty string. Postgres
// distinguishes the empty string from NULL — and several nullable
// columns have CHECK constraints (e.g. `chk_membership_types_frequency`)
// that explicitly reject empty strings. Converting empty strings to nil
// here unifies the two
// representations on the projector boundary, so neither side has to
// migrate. NOT NULL columns whose payload is genuinely `""` would still
// fail the constraint after this — but the existing enqueue code never
// sends `""` for any NOT NULL string column.
func nullifyEmptyString(v any) any {
	if s, ok := v.(string); ok && s == "" {
		return nil
	}
	return v
}

// coerceTimestamp turns an epoch-ms float (the sidecar's wire format) into
// a time.Time. Anything else (string, nil, time.Time) passes through —
// Postgres parses ISO strings into timestamptz natively.
func coerceTimestamp(v any) any {
	switch x := v.(type) {
	case float64:
		return time.UnixMilli(int64(x)).UTC()
	case int64:
		return time.UnixMilli(x).UTC()
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return time.UnixMilli(n).UTC()
		}
		return v
	default:
		return v
	}
}
