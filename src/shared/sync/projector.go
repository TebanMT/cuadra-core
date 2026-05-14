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

// projectors is the dispatch table. Every entry in SyncedTables must have a
// projector registered or push fails with `missing_projector` (ADR-008 §3.1
// — fail loud, do not degrade silently). Today every projector delegates to
// the same generic implementation; the map exists so future entity-specific
// behaviour (e.g. denormalisation, side effects) can replace one entry
// without touching the rest.
var projectors = func() map[string]Projector {
	m := make(map[string]Projector, len(SyncedTables))
	for i := range SyncedTables {
		t := SyncedTables[i]
		m[t.Type] = func(g *gorm.DB, gymID, entityID uuid.UUID, payload []byte) error {
			return projectGeneric(g, t, gymID, entityID, payload)
		}
	}
	return m
}()

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
		case isTimestampColumn(c):
			arg = coerceTimestamp(v)
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
// All known timestamp columns end in `_at`; date columns end in `_date` or
// `birthdate` and arrive as ISO date strings, which Postgres parses
// natively without conversion.
func isTimestampColumn(name string) bool {
	return strings.HasSuffix(name, "_at")
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
