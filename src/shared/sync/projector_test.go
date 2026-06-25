//go:build server

package sync

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProjector_AllSyncedTablesHaveAProjector(t *testing.T) {
	for _, et := range SyncedTables {
		if _, ok := projectors[et.Type]; !ok {
			t.Errorf("missing projector for entity_type=%s", et.Type)
		}
	}
}

func TestBuildProjectorUpsert_OmitsAbsentColumns(t *testing.T) {
	gymID := uuid.New()
	entityID := uuid.New()
	payload := json.RawMessage(`{"version":1,"email":"a@b.com","full_name":"X","role":"owner","active":true,"password_hash":"h","updated_at":1714060000000}`)

	tbl := *FindTable("users")
	q, args, err := buildProjectorUpsert(tbl, gymID, entityID, payload)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Required columns from the payload, plus injected id + gym_id.
	for _, want := range []string{"id", "gym_id", "version", "email", "full_name", "role", "active", "password_hash", "updated_at"} {
		if !strings.Contains(q, want) {
			t.Errorf("missing column %q in SQL: %s", want, q)
		}
	}
	// Columns not in payload (phone, must_change_password, last_login_at, created_by) must NOT appear — they keep their schema default on insert.
	for _, omitted := range []string{"phone", "must_change_password", "last_login_at", "created_by"} {
		if strings.Contains(q, omitted) {
			t.Errorf("unexpected column %q in SQL (should be omitted): %s", omitted, q)
		}
	}
	if !strings.Contains(q, "ON CONFLICT (id) DO UPDATE SET") {
		t.Errorf("missing ON CONFLICT clause: %s", q)
	}
	if strings.Contains(q, "id = EXCLUDED.id") {
		t.Errorf("id should not be in SET clause: %s", q)
	}
	// 9 columns × 1 arg each.
	if got := len(args); got != 9 {
		t.Errorf("args count = %d, want 9", got)
	}
}

// helper: ¿el UPSERT generado para un payload (ya pasado por la guarda)
// menciona la columna?
func upsertHasColumn(t *testing.T, payload []byte, col string) bool {
	t.Helper()
	tbl := *FindTable("users")
	q, _, err := buildProjectorUpsert(tbl, uuid.New(), uuid.New(), payload)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return strings.Contains(q, col)
}

func TestGuardUserCredential_StripsOwnerPasswordHash(t *testing.T) {
	// Regresión del bug "la contraseña del dueño deja de funcionar": un push
	// del sidecar trae el password_hash del owner (viejo/stale/de menor costo).
	// El cloud es la autoridad → la guarda lo descarta y el UPSERT lo omite,
	// así ON CONFLICT preserva el hash bueno del cloud.
	payload := json.RawMessage(`{"version":2,"email":"o@b.com","full_name":"X","role":"owner","active":true,"password_hash":"stale-or-weaker","updated_at":1714060000000}`)
	guarded, err := guardUserCredential(payload)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if upsertHasColumn(t, guarded, "password_hash") {
		t.Errorf("password_hash de owner debe quedar fuera del UPSERT")
	}
	// El resto del perfil sí debe proyectarse.
	for _, want := range []string{"email", "full_name", "active"} {
		if !upsertHasColumn(t, guarded, want) {
			t.Errorf("falta columna %q tras la guarda", want)
		}
	}
}

func TestGuardUserCredential_StripsEmptyHashForAnyRole(t *testing.T) {
	// El caso catastrófico: sidecar sin cached_login siembra password_hash=""
	// y lo empuja. Para cualquier rol, un hash vacío jamás debe tocar la fila
	// del cloud (si no, nullifyEmptyString lo colapsa a NULL y el login muere).
	for _, role := range []string{"owner", "operator", "manager"} {
		payload := json.RawMessage(`{"version":2,"email":"x@b.com","role":"` + role + `","active":true,"password_hash":"","updated_at":1}`)
		guarded, err := guardUserCredential(payload)
		if err != nil {
			t.Fatalf("guard %s: %v", role, err)
		}
		if upsertHasColumn(t, guarded, "password_hash") {
			t.Errorf("role=%s: password_hash vacío debe quedar fuera del UPSERT", role)
		}
	}
}

func TestGuardUserCredential_AllowsOperatorRealReset(t *testing.T) {
	// El reset legacy de operador desde recepción (UC-009, cmd/sidecar) SÍ debe
	// propagar: hash NO vacío + rol "operator" pasa intacto (incluso con
	// espacios/capitalización distinta, por EqualFold+TrimSpace).
	for _, role := range []string{"operator", "Operator", " operator "} {
		payload := json.RawMessage(`{"version":3,"email":"op@b.com","role":"` + role + `","active":true,"password_hash":"$2a$12$realnewhash","updated_at":1}`)
		guarded, err := guardUserCredential(payload)
		if err != nil {
			t.Fatalf("guard %q: %v", role, err)
		}
		if !upsertHasColumn(t, guarded, "password_hash") {
			t.Errorf("role=%q: reset real de operador debe propagar password_hash", role)
		}
	}
}

func TestGuardUserCredential_FailsClosedOnUnknownOrMissingRole(t *testing.T) {
	// Fail-closed: un hash NO vacío con rol ausente o desconocido NO debe
	// escribirse — sólo el rol "operator" pasa. Protege contra writers futuros
	// y el backfill Project() si el CHECK de rol upstream cambiara.
	cases := []string{
		`{"version":2,"email":"x@b.com","active":true,"password_hash":"$2a$12$h","updated_at":1}`,                  // rol ausente
		`{"version":2,"email":"x@b.com","role":"manager","active":true,"password_hash":"$2a$12$h","updated_at":1}`, // rol desconocido
		`{"version":2,"email":"x@b.com","role":null,"active":true,"password_hash":"$2a$12$h","updated_at":1}`,      // rol null
	}
	for _, c := range cases {
		guarded, err := guardUserCredential(json.RawMessage(c))
		if err != nil {
			t.Fatalf("guard %s: %v", c, err)
		}
		if upsertHasColumn(t, guarded, "password_hash") {
			t.Errorf("fail-closed: password_hash debe descartarse para %s", c)
		}
	}
}

func TestGuardUserCredential_NeverTouchesPinHash(t *testing.T) {
	// pin_hash es autoridad del sidecar (se asigna en recepción) — la guarda
	// no lo toca aunque descarte password_hash.
	payload := json.RawMessage(`{"version":2,"email":"o@b.com","role":"owner","active":true,"password_hash":"x","pin_hash":"$2a$12$pin","updated_at":1}`)
	guarded, err := guardUserCredential(payload)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if upsertHasColumn(t, guarded, "password_hash") {
		t.Errorf("password_hash de owner debe quedar fuera")
	}
	if !upsertHasColumn(t, guarded, "pin_hash") {
		t.Errorf("pin_hash debe preservarse (autoridad del sidecar)")
	}
}

func TestBuildProjectorUpsert_PinsGymAndIDFromContext(t *testing.T) {
	gymID := uuid.New()
	entityID := uuid.New()
	// Adversarial payload claims wrong gym_id and id — the projector must
	// overwrite both with the upsert context.
	body := map[string]any{
		"id":            "ffffffff-ffff-ffff-ffff-ffffffffffff",
		"gym_id":        "ffffffff-ffff-ffff-ffff-ffffffffffff",
		"version":       1,
		"email":         "a@b.com",
		"full_name":     "X",
		"role":          "owner",
		"active":        true,
		"password_hash": "h",
	}
	payload, _ := json.Marshal(body)
	tbl := *FindTable("users")
	_, args, err := buildProjectorUpsert(tbl, gymID, entityID, payload)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// id is always the first column emitted because it appears first in
	// SyncedTables.Columns. gym_id is second.
	if got := args[0]; got != entityID.String() {
		t.Errorf("id arg = %v, want %s", got, entityID)
	}
	if got := args[1]; got != gymID.String() {
		t.Errorf("gym_id arg = %v, want %s", got, gymID)
	}
}

func TestBuildProjectorUpsert_JSONBCastsAndReMarshals(t *testing.T) {
	gymID := uuid.New()
	body := map[string]any{
		"id":              gymID.String(),
		"gym_id":          gymID.String(),
		"version":         1,
		"name":            "G",
		"payment_methods": []string{"cash", "card"},
	}
	payload, _ := json.Marshal(body)
	tbl := *FindTable("gyms")
	q, args, err := buildProjectorUpsert(tbl, gymID, gymID, payload)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(q, "?::jsonb") {
		t.Errorf("expected ::jsonb cast in SQL: %s", q)
	}
	// Find the payment_methods arg — it should be a JSON-encoded string.
	var found bool
	for _, a := range args {
		if s, ok := a.(string); ok && strings.Contains(s, "cash") && strings.Contains(s, "card") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("payment_methods not re-marshalled to JSON string: %v", args)
	}
}

func TestBuildProjectorUpsert_TimestampMsConvertedToTime(t *testing.T) {
	gymID := uuid.New()
	entityID := uuid.New()
	when := time.UnixMilli(1714060000000).UTC()
	body := map[string]any{
		"id":            entityID.String(),
		"gym_id":        gymID.String(),
		"version":       1,
		"email":         "a@b.com",
		"full_name":     "X",
		"role":          "owner",
		"active":        true,
		"password_hash": "h",
		"updated_at":    when.UnixMilli(),
	}
	payload, _ := json.Marshal(body)
	tbl := *FindTable("users")
	_, args, err := buildProjectorUpsert(tbl, gymID, entityID, payload)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// updated_at is the last column emitted — find by type rather than position.
	found := false
	for _, a := range args {
		if got, ok := a.(time.Time); ok {
			if !got.Equal(when) {
				t.Errorf("timestamp coerced to wrong instant: got %v want %v", got, when)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("expected at least one time.Time arg, got %T values", args)
	}
}

// TestBuildProjectorUpsert_NonAtTimestampsCoerced — regresión del bug
// "cannot find encode plan for OID 1184" en challenges: las columnas
// measurement_t0_deadline y measurement_t1_start son TIMESTAMPTZ pero no
// terminan en `_at`, y antes la heurística por sufijo las dejaba pasar como
// int64 crudo al driver pgx. timestamptzColumns ahora las cataloga
// explícitamente.
func TestBuildProjectorUpsert_NonAtTimestampsCoerced(t *testing.T) {
	cases := []struct {
		table  string
		column string
		extras map[string]any
	}{
		{
			table:  "challenges",
			column: "measurement_t0_deadline",
			extras: map[string]any{
				"name":                    "Reto Test",
				"starts_at":               time.UnixMilli(1714060000000).UnixMilli(),
				"measurement_t0_deadline": int64(1779926400000),
				"measurement_t1_start":    int64(1779926400000),
				"ends_at":                 int64(1781136000000),
				"status":                  "active",
				"inscription_fee_cents":   int64(20000),
				"inscription_refundable":  true,
				"min_weekly_attendance":   3,
				"attendance_grace_weeks":  2,
				"strength_cap_pct":        25,
				"tie_margin_ir":           5,
				"bf_floor_male_pct":       6,
				"bf_floor_female_pct":     14,
			},
		},
		{
			table:  "challenges",
			column: "measurement_t1_start",
			extras: map[string]any{
				"name":                    "Reto Test",
				"starts_at":               int64(1714060000000),
				"measurement_t0_deadline": int64(1779926400000),
				"measurement_t1_start":    int64(1779926400000),
				"ends_at":                 int64(1781136000000),
				"status":                  "active",
				"inscription_fee_cents":   int64(20000),
				"inscription_refundable":  true,
				"min_weekly_attendance":   3,
				"attendance_grace_weeks":  2,
				"strength_cap_pct":        25,
				"tie_margin_ir":           5,
				"bf_floor_male_pct":       6,
				"bf_floor_female_pct":     14,
			},
		},
		{
			table:  "notification_queue",
			column: "scheduled_for",
			extras: map[string]any{
				"channel":       "whatsapp",
				"template":      "expiring_soon",
				"payload":       map[string]any{},
				"scheduled_for": int64(1779926400000),
				"status":        "pending",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			gymID := uuid.New()
			entityID := uuid.New()
			body := map[string]any{
				"id":      entityID.String(),
				"gym_id":  gymID.String(),
				"version": 1,
			}
			for k, v := range tc.extras {
				body[k] = v
			}
			payload, _ := json.Marshal(body)
			tbl := FindTable(tc.table)
			if tbl == nil {
				t.Fatalf("table %q not registered in SyncedTables", tc.table)
			}
			cols, args, err := buildProjectorUpsert(*tbl, gymID, entityID, payload)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			// Find the arg at the slot matching tc.column.
			idx := -1
			split := strings.SplitN(cols[strings.Index(cols, "("):], ",", -1)
			_ = split
			// Easier: re-parse the columns out of the SQL header to locate idx.
			start := strings.Index(cols, "(") + 1
			end := strings.Index(cols, ")")
			colList := strings.Split(cols[start:end], ", ")
			for i, c := range colList {
				if c == tc.column {
					idx = i
					break
				}
			}
			if idx < 0 {
				t.Fatalf("column %q not in projected columns: %v", tc.column, colList)
			}
			if _, ok := args[idx].(time.Time); !ok {
				t.Errorf("%s.%s arg type = %T, want time.Time (raw int would fail pgx encode for OID 1184)",
					tc.table, tc.column, args[idx])
			}
		})
	}
}

func TestBuildProjectorUpsert_ByteaBase64Decoded(t *testing.T) {
	gymID := uuid.New()
	memberID := uuid.New()
	fpID := uuid.New()
	raw := []byte{0x01, 0x02, 0x03, 0xff}
	body := map[string]any{
		"id":                 fpID.String(),
		"gym_id":             gymID.String(),
		"version":            1,
		"member_id":          memberID.String(),
		"template_encrypted": base64.StdEncoding.EncodeToString(raw),
		"template_format":    "dp_uareu",
		"registered_by":      uuid.New().String(),
	}
	payload, _ := json.Marshal(body)
	tbl := *FindTable("member_fingerprints")
	_, args, err := buildProjectorUpsert(tbl, gymID, fpID, payload)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var foundBytes bool
	for _, a := range args {
		if b, ok := a.([]byte); ok && string(b) == string(raw) {
			foundBytes = true
		}
	}
	if !foundBytes {
		t.Errorf("template_encrypted not decoded to []byte: %v", args)
	}
}

func TestBuildProjectorUpsert_DeletedAtMsAlias(t *testing.T) {
	gymID := uuid.New()
	memberID := uuid.New()
	fpID := uuid.New()
	deletedMs := time.Now().UTC().UnixMilli()
	body := map[string]any{
		"id":                 fpID.String(),
		"gym_id":             gymID.String(),
		"version":            2,
		"member_id":          memberID.String(),
		"template_encrypted": base64.StdEncoding.EncodeToString([]byte("x")),
		"template_format":    "dp_uareu",
		"registered_by":      uuid.New().String(),
		"deleted_at_ms":      deletedMs,
	}
	payload, _ := json.Marshal(body)
	tbl := *FindTable("member_fingerprints")
	q, args, err := buildProjectorUpsert(tbl, gymID, fpID, payload)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(q, "deleted_at") {
		t.Errorf("deleted_at column missing: %s", q)
	}
	if strings.Contains(q, "deleted_at_ms") {
		t.Errorf("alias not stripped — deleted_at_ms appears in SQL: %s", q)
	}
	// Must include a time.Time arg for the deleted_at column.
	var foundTime bool
	for _, a := range args {
		if _, ok := a.(time.Time); ok {
			foundTime = true
		}
	}
	if !foundTime {
		t.Errorf("deleted_at not coerced to time.Time: %v", args)
	}
}

// TestBuildProjectorUpsert_EmptyStringBecomesNull — regression for the
// `chk_membership_types_frequency` constraint failure. The sidecar's
// enqueue helpers default nullable *string fields to "" when nil; the
// projector must collapse those to SQL NULL or Postgres rejects them.
func TestBuildProjectorUpsert_EmptyStringBecomesNull(t *testing.T) {
	gymID := uuid.New()
	mtID := uuid.New()
	body := map[string]any{
		"id":                    mtID.String(),
		"gym_id":                gymID.String(),
		"version":               1,
		"name":                  "Estudiante",
		"price":                 400,
		"duration_days":         30,
		"enrollment_fee":        0,
		"maintenance_fee":       0,
		"maintenance_frequency": "", // sidecar default for nil pointer
		"active":                true,
	}
	payload, _ := json.Marshal(body)
	tbl := *FindTable("membership_types")
	_, args, err := buildProjectorUpsert(tbl, gymID, mtID, payload)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Find the maintenance_frequency arg (it's where columns line up).
	for i, c := range tbl.Columns {
		if c != "maintenance_frequency" {
			continue
		}
		// args index aligns with cols (id+gym_id+others present in payload).
		// Walk by name lookup: count present payload columns up to this one.
		var emittedIdx int
		for _, cc := range tbl.Columns[:i+1] {
			if _, ok := body[cc]; ok || cc == "id" || cc == "gym_id" {
				emittedIdx++
			}
		}
		got := args[emittedIdx-1]
		if got != nil {
			t.Errorf("expected maintenance_frequency to be nil (sql NULL), got %v (%T)", got, got)
		}
		return
	}
	t.Errorf("maintenance_frequency column not found in table registry")
}

func TestProject_MissingProjectorErrors(t *testing.T) {
	// Direct dispatch with an unknown type — the handler validates against
	// SyncedTables before we get here in production, but the projector must
	// still refuse silently.
	err := project(nil, "made_up_table", uuid.New(), uuid.New(), []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "missing_projector") {
		t.Errorf("expected missing_projector error, got %v", err)
	}
}

func TestBuildProjectorUpsert_EmptyPayloadErrors(t *testing.T) {
	tbl := *FindTable("users")
	_, _, err := buildProjectorUpsert(tbl, uuid.New(), uuid.New(), []byte(`{}`))
	if err != nil {
		// id + gym_id are forced from context, so 2 cols always present —
		// this should NOT error. Sanity check the invariant.
		t.Errorf("forced id+gym_id should keep payload non-empty, got err=%v", err)
	}
}

func TestBuildProjectorUpsert_AllRegisteredTablesProduceValidSQL(t *testing.T) {
	gymID := uuid.New()
	entityID := uuid.New()
	for _, et := range SyncedTables {
		t.Run(et.Type, func(t *testing.T) {
			// Composite-key tables don't have an `id` column; their natural
			// key columns must appear in the payload for the INSERT to
			// satisfy NOT NULL constraints. The projector's auto-pin of
			// gym_id covers half of (gym_id, alert_key) — we add the rest
			// of the conflict key here so the test keeps the same shape
			// as the surrogate-id case.
			payload := []byte(`{"version":1}`)
			if len(et.CompositeKey) > 0 {
				extra := map[string]any{"version": 1}
				for _, k := range et.CompositeKey {
					if k == "gym_id" || k == "id" {
						continue
					}
					extra[k] = "stub"
				}
				payload = mustJSON(t, extra)
			}
			q, args, err := buildProjectorUpsert(et, gymID, entityID, payload)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if !strings.HasPrefix(q, "INSERT INTO "+et.Table+" ") {
				t.Errorf("unexpected SQL prefix: %s", q)
			}
			minArgs := 3 // id, gym_id, version
			expectedConflict := "ON CONFLICT (id) DO UPDATE SET"
			if len(et.CompositeKey) > 0 {
				minArgs = len(et.CompositeKey) + 1 // composite key + version
				expectedConflict = "ON CONFLICT (" + strings.Join(et.CompositeKey, ", ") + ") DO UPDATE SET"
			}
			if len(args) < minArgs {
				t.Errorf("expected at least %d args, got %d", minArgs, len(args))
			}
			if !strings.Contains(q, expectedConflict) {
				t.Errorf("missing %q clause: %s", expectedConflict, q)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	return b
}
