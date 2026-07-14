//go:build sidecar

package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func TestClassifyStuckError(t *testing.T) {
	cases := []struct {
		in       string
		wantKind string
		wantMsg  string
	}{
		{
			// Cloud nuevo: prefijo de status + mensaje ya legible.
			in:       `rejected_duplicate: Ya existe un plan llamado "Mensual" en la nube (quizá creado desde otro equipo). Renombra el tuyo para destrabar la sincronización.`,
			wantKind: StuckKindDuplicate,
			wantMsg:  `Ya existe un plan llamado "Mensual" en la nube (quizá creado desde otro equipo). Renombra el tuyo para destrabar la sincronización.`,
		},
		{
			// Cloud viejo: error crudo de Postgres — clasifica igual (el CTA
			// del desktop funciona) aunque el texto quede crudo.
			in:       `ERROR: duplicate key value violates unique constraint "uq_membership_types_gym_name" (SQLSTATE 23505)`,
			wantKind: StuckKindDuplicate,
			wantMsg:  `ERROR: duplicate key value violates unique constraint "uq_membership_types_gym_name" (SQLSTATE 23505)`,
		},
		{
			in:       "rejected_unknown_entity_type: unknown entity_type: promos",
			wantKind: StuckKindOther,
			wantMsg:  "rejected_unknown_entity_type: unknown entity_type: promos",
		},
		{
			in:       "insert or update on table violates foreign key constraint",
			wantKind: StuckKindOther,
			wantMsg:  "insert or update on table violates foreign key constraint",
		},
		{in: "", wantKind: StuckKindOther, wantMsg: ""},
	}
	for _, c := range cases {
		kind, msg := classifyStuckError(c.in)
		if kind != c.wantKind || msg != c.wantMsg {
			t.Errorf("classifyStuckError(%q) = (%q, %q), want (%q, %q)", c.in, kind, msg, c.wantKind, c.wantMsg)
		}
	}
}

func TestEntityLabelFromPayload(t *testing.T) {
	cases := []struct {
		payload string
		want    string
	}{
		{`{"name":"Mensual","price":500}`, "Mensual"},
		{`{"full_name":"Juan Pérez"}`, "Juan Pérez"},
		{`{"folio":"PAGO/123","amount":500}`, "PAGO/123"},
		{`{"template_key":"payment_receipt"}`, "payment_receipt"},
		{`{"name":"  "}`, ""},         // whitespace no cuenta como label
		{`{"price":500}`, ""},         // sin campo conocido
		{`no es json`, ""},            // payload roto no truena
		{`{"name":" Agua "}`, "Agua"}, // se recorta
	}
	for _, c := range cases {
		if got := entityLabelFromPayload([]byte(c.payload)); got != c.want {
			t.Errorf("entityLabelFromPayload(%q) = %q, want %q", c.payload, got, c.want)
		}
	}
}

func mtQueuePayload(gymID uuid.UUID, id uuid.UUID, name string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": id.String(), "gym_id": gymID.String(), "version": 1,
		"created_at": time.Now().UTC().UnixMilli(), "updated_at": time.Now().UTC().UnixMilli(),
		"name": name, "price": 500, "duration_days": 30,
		"enrollment_fee": 0, "maintenance_fee": 0, "active": true,
	})
	return b
}

// Round-trip sidecar-side del flujo "rechazo permanente → rename → destrabado"
// sobre las migraciones SQLite reales:
//
//  1. se encola un plan "Mensual" (SqliteQueue.Enqueue, como lo haría el repo)
//  2. el server lo rechaza rejected_duplicate N veces → la fila queda ATORADA
//     y /sync/status la expone clasificada con label humano
//  3. el operador renombra → Enqueue coalescea el MISMO row (payload nuevo,
//     mismo queue_id) — esto es lo que hace que el rename destrabe
//  4. el server acepta el push del payload renombrado → synced_at set,
//     stuck desaparece del status
func TestStuckDuplicate_RenameUnsticksRoundTrip(t *testing.T) {
	gymID := uuid.New()
	db, uow := compositeKeyTestDB(t, gymID)
	a := NewAgent(AgentConfig{BaseURL: "http://unused"}, db, uow)
	ctx := context.Background()

	planID := uuid.New()
	q := NewSqliteQueue()
	if err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		return q.Enqueue(ctx, tx, "membership_types", planID.String(), "upsert", mtQueuePayload(gymID, planID, "Mensual"), 1)
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	batch, err := a.takeBatch(ctx)
	if err != nil || len(batch) != 1 {
		t.Fatalf("takeBatch: %v (%d items)", err, len(batch))
	}
	queueID := batch[0].QueueID

	// El cloud rechaza el duplicado hasta cruzar el umbral de atorado.
	dupMsg := `Ya existe un plan llamado "Mensual" en la nube (quizá creado desde otro equipo). Renombra el tuyo para destrabar la sincronización.`
	reject := &PushResponse{Results: []PushItemResult{{
		QueueID: queueID, EntityID: planID.String(),
		Status: StatusRejectedDuplicate, Error: dupMsg,
	}}}
	for i := 0; i < stuckPushThreshold; i++ {
		if err := a.handlePushResponse(ctx, batch, reject); err != nil {
			t.Fatalf("handlePushResponse reject #%d: %v", i+1, err)
		}
	}

	a.refreshPendingCount(ctx)
	snap := a.Snapshot()
	if snap.StuckPushCount != 1 {
		t.Fatalf("StuckPushCount = %d, want 1", snap.StuckPushCount)
	}
	if len(snap.StuckItems) != 1 {
		t.Fatalf("StuckItems = %+v, want 1 item", snap.StuckItems)
	}
	it := snap.StuckItems[0]
	if it.Kind != StuckKindDuplicate {
		t.Errorf("Kind = %q, want duplicate", it.Kind)
	}
	if it.EntityType != "membership_types" || it.EntityID != planID.String() {
		t.Errorf("identidad = %s/%s, want membership_types/%s", it.EntityType, it.EntityID, planID)
	}
	if it.Message != dupMsg {
		t.Errorf("Message = %q (el prefijo de status debe quitarse)", it.Message)
	}
	if it.EntityLabel != "Mensual" {
		t.Errorf("EntityLabel = %q, want Mensual", it.EntityLabel)
	}
	if it.RetryCount != stuckPushThreshold {
		t.Errorf("RetryCount = %d, want %d", it.RetryCount, stuckPushThreshold)
	}
	// StuckPushError (la muestra del detalle) también sale limpio, sin prefijo.
	if snap.StuckPushError != dupMsg {
		t.Errorf("StuckPushError = %q, want mensaje limpio", snap.StuckPushError)
	}

	// El wire de /sync/status expone el detalle y mantiene sync_error.
	resp := buildStatusResponse(snap, time.Now().UTC())
	if resp.State != StateSyncError {
		t.Errorf("State = %q, want %q", resp.State, StateSyncError)
	}
	if len(resp.QueueStuckItems) != 1 || resp.QueueStuckItems[0].Kind != StuckKindDuplicate {
		t.Errorf("QueueStuckItems = %+v", resp.QueueStuckItems)
	}

	// Rename en el desktop: el repo re-encola el MISMO entity — coalescing
	// sobre la fila pendiente (mismo queue_id, payload nuevo).
	if err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		return q.Enqueue(ctx, tx, "membership_types", planID.String(), "upsert", mtQueuePayload(gymID, planID, "Mensual recepción"), 2)
	}); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	var pendingRows int
	if err := db.Get(&pendingRows, `SELECT COUNT(*) FROM sync_queue WHERE synced_at IS NULL`); err != nil {
		t.Fatalf("count: %v", err)
	}
	if pendingRows != 1 {
		t.Fatalf("el rename debe COALESCEAR, no apilar: %d filas pendientes", pendingRows)
	}

	batch2, err := a.takeBatch(ctx)
	if err != nil || len(batch2) != 1 {
		t.Fatalf("takeBatch 2: %v (%d)", err, len(batch2))
	}
	if batch2[0].QueueID != queueID {
		t.Errorf("queue_id cambió tras el rename: %s → %s", queueID, batch2[0].QueueID)
	}
	var pl map[string]any
	_ = json.Unmarshal(batch2[0].Payload, &pl)
	if pl["name"] != "Mensual recepción" {
		t.Errorf("payload re-encolado name=%v, want el nombre nuevo", pl["name"])
	}

	// El push del payload renombrado entra limpio → la fila sale de la cola.
	now := time.Now().UTC()
	accept := &PushResponse{Results: []PushItemResult{{
		QueueID: queueID, EntityID: planID.String(),
		Status: StatusAccepted, ServerVersion: 2, ServerUpdatedAt: &now,
	}}}
	if err := a.handlePushResponse(ctx, batch2, accept); err != nil {
		t.Fatalf("handlePushResponse accept: %v", err)
	}
	a.refreshPendingCount(ctx)
	snap = a.Snapshot()
	if snap.PendingCount != 0 || snap.StuckPushCount != 0 || len(snap.StuckItems) != 0 {
		t.Errorf("tras aceptar: pending=%d stuck=%d items=%d, want 0/0/0",
			snap.PendingCount, snap.StuckPushCount, len(snap.StuckItems))
	}
	var lastErr *string
	if err := db.Get(&lastErr, `SELECT last_error FROM sync_queue WHERE id = ?`, queueID); err != nil {
		t.Fatalf("last_error: %v", err)
	}
	if lastErr != nil {
		t.Errorf("last_error = %q, want NULL tras el éxito", *lastErr)
	}
}

// Un batch COMPLETO donde el server rechaza todos los items no debe re-tomarse
// en caliente dentro del mismo Push(): sin progreso, re-leer la cola devuelve
// exactamente el mismo batch y el loop martillaría al cloud para siempre.
func TestPush_FullBatchRejectedExitsWithoutHotLoop(t *testing.T) {
	gymID := uuid.New()
	db, uow := compositeKeyTestDB(t, gymID)

	var pushCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := pushCount.Add(1)
		if n > 5 {
			// Red de seguridad: si el guard regresiona, el loop recibe 5xx y
			// Push devuelve error en vez de colgar el test hasta el timeout.
			http.Error(w, "hot loop detected", http.StatusInternalServerError)
			return
		}
		var req PushRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		resp := PushResponse{ServerNow: time.Now().UTC(), SchemaVersion: SchemaVersion}
		for _, it := range req.Batch {
			resp.Results = append(resp.Results, PushItemResult{
				QueueID: it.QueueID, EntityID: it.EntityID,
				Status: StatusRejectedDuplicate,
				Error:  fmt.Sprintf("Ya existe un plan llamado %q en la nube.", "Mensual"),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	// 2 filas con BatchSize=2 → el batch va LLENO (la condición del loop).
	q := NewSqliteQueue()
	ctx := context.Background()
	if err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		for i := 0; i < 2; i++ {
			id := uuid.New()
			if err := q.Enqueue(ctx, tx, "membership_types", id.String(), "upsert", mtQueuePayload(gymID, id, "Mensual"), 1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	a := NewAgent(AgentConfig{BaseURL: srv.URL, BatchSize: 2}, db, uow)
	if err := a.Push(ctx); err != nil {
		t.Fatalf("Push: %v (¿el guard regresionó y el loop llegó al 5xx?)", err)
	}
	if got := pushCount.Load(); got != 1 {
		t.Errorf("requests al cloud = %d, want 1 (batch completo sin progreso sale del loop)", got)
	}

	snap := a.Snapshot()
	if snap.PendingCount != 2 {
		t.Errorf("PendingCount = %d, want 2 (las filas siguen en cola para el próximo tick)", snap.PendingCount)
	}
	var maxRetry int
	if err := db.Get(&maxRetry, `SELECT MAX(retry_count) FROM sync_queue`); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if maxRetry != 1 {
		t.Errorf("retry_count = %d, want 1 (un solo intento en este ciclo)", maxRetry)
	}
}
