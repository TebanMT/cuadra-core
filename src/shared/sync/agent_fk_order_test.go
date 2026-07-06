//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// Estos tests pinean el bug del full-sync v1.0.6: un gym con renovaciones
// de membresía (cadenas replaced_by) moría con "full-sync: FOREIGN KEY
// constraint failed" en instalaciones frescas. Cadena causal:
//
//  1. Renew marca la membresía VIEJA con replaced_by = <id de la NUEVA> y
//     crea la nueva en la misma tx → ambas llegan a sync_entities con
//     server_updated_at a microsegundos.
//  2. ListForFullSync ordena por (server_updated_at, entity_id) — ciego a
//     la dirección de la FK. Si la vieja se aplica primero, su INSERT
//     viola la self-FK memberships.replaced_by.
//  3. El apply corría con foreign_keys=ON inmediatas dentro de una tx por
//     página → fallo determinista en cada retry (el cursor re-lee el
//     mismo corte).
//
// Fix: ApplyPullPage difiere las FKs al COMMIT (defer_foreign_keys) y, si
// el límite de página parte la cadena, Pull/FullSync reintentan la página
// unida con la siguiente.

// seedMemberAndType siembra las filas padre que una membresía necesita
// (user → member + membership_type), estilo setupSidecarDB.
func seedMemberAndType(t *testing.T, db *sqlx.DB, gymID uuid.UUID) (memberID, typeID uuid.UUID) {
	t.Helper()
	now := time.Now().UTC().UnixMilli()
	userID := uuid.New()
	if _, err := db.Exec(`
		INSERT INTO users (id, gym_id, version, created_at, updated_at,
		                   email, password_hash, full_name, role, active)
		VALUES (?, ?, 1, ?, ?, 'op@gym.com', 'x', 'Op', 'operator', 1)`,
		userID, gymID, now, now); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	memberID = uuid.New()
	if _, err := db.Exec(`
		INSERT INTO members (id, gym_id, version, created_at, updated_at,
		                     folio, full_name, phone, status, created_by)
		VALUES (?, ?, 1, ?, ?, ?, 'Socio Renovador', '5555555', 'active', ?)`,
		memberID, gymID, now, now, "M-"+memberID.String()[:8], userID); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	typeID = uuid.New()
	if _, err := db.Exec(`
		INSERT INTO membership_types (id, gym_id, version, created_at, updated_at,
		                              name, price, duration_days, duration_months)
		VALUES (?, ?, 1, ?, ?, 'Mensual', 50000, 30, 1)`,
		typeID, gymID, now, now); err != nil {
		t.Fatalf("seed membership_type: %v", err)
	}
	return memberID, typeID
}

// renewalChain construye el par de PullChanges de una renovación en el
// ORDEN ADVERSO real (vieja con replaced_by primero, nueva después) — el
// orden en que ListForFullSync las emite cuando la vieja recibió el
// server_updated_at menor.
func renewalChain(t *testing.T, gymID, memberID, typeID uuid.UUID) (old, renewed syncpkg.PullChange, oldID, newID uuid.UUID) {
	t.Helper()
	oldID, newID = uuid.New(), uuid.New()
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	mk := func(id uuid.UUID, version int, status string, start, expiry string, replacedBy *uuid.UUID, ts time.Time) syncpkg.PullChange {
		pl := map[string]any{
			"id":                       id.String(),
			"gym_id":                   gymID.String(),
			"version":                  version,
			"created_at":               base.UnixMilli(),
			"updated_at":               ts.UnixMilli(),
			"member_id":                memberID.String(),
			"membership_type_id":       typeID.String(),
			"type_name_snapshot":       "Mensual",
			"price_snapshot":           500.0, // wire en pesos
			"duration_days_snapshot":   30,
			"duration_months_snapshot": 1,
			"start_date":               start,
			"expiry_date":              expiry,
			"status":                   status,
		}
		if replacedBy != nil {
			pl["replaced_by"] = replacedBy.String()
		}
		b, err := json.Marshal(pl)
		if err != nil {
			t.Fatalf("marshal membership: %v", err)
		}
		return syncpkg.PullChange{
			EntityType: "memberships", EntityID: id.String(),
			Version: version, Payload: b, ServerUpdatedAt: ts,
		}
	}
	// La vieja (versión 2: create + el Renew que le puso replaced_by) con
	// timestamp UN microsegundo antes que la nueva — adyacentes, como las
	// estampa UpsertOne al procesar el batch del push.
	old = mk(oldID, 2, "replaced", "2026-06-01", "2026-07-01", &newID, base)
	renewed = mk(newID, 1, "active", "2026-07-01", "2026-08-01", nil, base.Add(time.Microsecond))
	return old, renewed, oldID, newID
}

// assertChainLanded verifica que ambas membresías aterrizaron y la cadena
// quedó íntegra (vieja "reemplazada" apuntando a la nueva activa).
func assertChainLanded(t *testing.T, db *sqlx.DB, oldID, newID uuid.UUID) {
	t.Helper()
	var row struct {
		Status     string  `db:"status"`
		ReplacedBy *string `db:"replaced_by"`
	}
	if err := db.Get(&row, `SELECT status, replaced_by FROM memberships WHERE id = ?`, oldID); err != nil {
		t.Fatalf("leer membresía vieja: %v", err)
	}
	if row.Status != "replaced" || row.ReplacedBy == nil || *row.ReplacedBy != newID.String() {
		t.Errorf("vieja: status=%q replaced_by=%v, want replaced→%s", row.Status, row.ReplacedBy, newID)
	}
	if err := db.Get(&row, `SELECT status, replaced_by FROM memberships WHERE id = ?`, newID); err != nil {
		t.Fatalf("leer membresía nueva: %v", err)
	}
	if row.Status != "active" || row.ReplacedBy != nil {
		t.Errorf("nueva: status=%q replaced_by=%v, want active sin replaced_by", row.Status, row.ReplacedBy)
	}
}

// TestApplyPullPage_CadenaRenovacion_OrdenAdverso — el escenario EXACTO del
// bug, en una sola página. Fase 1 pinea la causa raíz (sin defer, el orden
// adverso viola la FK — si esta fase deja de fallar es que la self-FK
// desapareció del schema y el test perdió sentido). Fase 2 verifica el fix.
func TestApplyPullPage_CadenaRenovacion_OrdenAdverso(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	memberID, typeID := seedMemberAndType(t, db, gymID)
	old, renewed, oldID, newID := renewalChain(t, gymID, memberID, typeID)
	ctx := context.Background()

	// Fase 1 — pin pre-fix: aplicar [vieja, nueva] con FKs inmediatas (el
	// código de v1.0.6) truena en el INSERT de la vieja. La tx rollbackea,
	// así que la fase 2 arranca limpia.
	rawErr := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		for _, ch := range []syncpkg.PullChange{old, renewed} {
			if err := syncpkg.ApplyPullChange(ctx, tx, ch); err != nil {
				return err
			}
		}
		return nil
	})
	if rawErr == nil || !strings.Contains(rawErr.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("pin pre-fix: quería FOREIGN KEY constraint failed, got %v", rawErr)
	}

	// Fase 2 — con ApplyPullPage (FKs diferidas) el mismo orden pasa.
	if err := syncpkg.ApplyPullPage(ctx, uow, []syncpkg.PullChange{old, renewed}, nil); err != nil {
		t.Fatalf("ApplyPullPage con cadena en orden adverso: %v", err)
	}
	assertChainLanded(t, db, oldID, newID)
}

// TestApplyPullPage_ErrorConContexto — regla de la casa: nunca más un error
// opaco. Un fallo inmediato (NOT NULL) debe decir entity_type/entity_id y
// versión.
func TestApplyPullPage_ErrorConContexto(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	_ = db

	memberID := uuid.New()
	// Payload sin full_name (NOT NULL sin default) → INSERT truena al tiro.
	pl, _ := json.Marshal(map[string]any{
		"id":         memberID.String(),
		"gym_id":     gymID.String(),
		"version":    1,
		"created_at": time.Now().UTC().UnixMilli(),
		"updated_at": time.Now().UTC().UnixMilli(),
		"folio":      "M-CTX",
		"status":     "active",
	})
	err := syncpkg.ApplyPullPage(context.Background(), uow, []syncpkg.PullChange{{
		EntityType: "members", EntityID: memberID.String(),
		Version: 1, Payload: pl, ServerUpdatedAt: time.Now().UTC(),
	}}, nil)
	if err == nil {
		t.Fatal("quería error por NOT NULL, got nil")
	}
	want := fmt.Sprintf("apply members/%s (version 1)", memberID)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error sin contexto de fila: %v (quería que contuviera %q)", err, want)
	}
}

// scriptedCloud sirve /full y /pull con páginas GUIONADAS — a diferencia de
// fakeCloud (una página, orden de map aleatorio) acá el orden y los cortes
// de página son deterministas, que es justo lo que estos tests ejercitan.
type scriptedCloud struct {
	fullPages map[string]syncpkg.FullSyncResponse // key = cursor ("" la primera)
	pullPages map[string]syncpkg.PullResponse     // key = since (RFC3339Nano, "" la primera)
}

func (s *scriptedCloud) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/sync/full", func(w http.ResponseWriter, r *http.Request) {
		resp, ok := s.fullPages[r.URL.Query().Get("cursor")]
		if !ok {
			http.Error(w, "cursor desconocido: "+r.URL.Query().Get("cursor"), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/api/v1/sync/pull", func(w http.ResponseWriter, r *http.Request) {
		resp, ok := s.pullPages[r.URL.Query().Get("since")]
		if !ok {
			http.Error(w, "since desconocido: "+r.URL.Query().Get("since"), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newScriptedAgent(t *testing.T, srv *httptest.Server, db *sqlx.DB, uow sharedDomain.UnitOfWork) *syncpkg.Agent {
	t.Helper()
	a := syncpkg.NewAgent(syncpkg.AgentConfig{
		BaseURL:          srv.URL,
		Interval:         24 * time.Hour, // los tests llaman FullSync/Pull directo
		BatchSize:        50,
		PullPageSize:     100,
		FullSyncPageSize: 200,
		HTTPClient:       srv.Client(),
	}, db, uow)
	a.SetToken("test-token")
	if err := a.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return a
}

// TestFullSync_CadenaPartidaEntrePaginas — el edge case: el límite de
// página cae JUSTO entre la vieja y la nueva. defer_foreign_keys solo no
// salva el commit de la primera página; FullSync debe reintentar la página
// unida con la siguiente en vez de fallar determinista.
func TestFullSync_CadenaPartidaEntrePaginas(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	memberID, typeID := seedMemberAndType(t, db, gymID)
	old, renewed, oldID, newID := renewalChain(t, gymID, memberID, typeID)

	serverNow := time.Date(2026, 7, 4, 12, 30, 0, 0, time.UTC)
	cloud := &scriptedCloud{fullPages: map[string]syncpkg.FullSyncResponse{
		"": {Changes: []syncpkg.PullChange{old}, NextCursor: "p2",
			HasMore: true, ServerNow: serverNow, SchemaVersion: syncpkg.SchemaVersion},
		"p2": {Changes: []syncpkg.PullChange{renewed},
			HasMore: false, ServerNow: serverNow, SchemaVersion: syncpkg.SchemaVersion},
	}}
	a := newScriptedAgent(t, cloud.serve(t), db, uow)

	if err := a.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync con cadena partida entre páginas: %v", err)
	}
	assertChainLanded(t, db, oldID, newID)

	// El full-sync finalizó: initial_sync_completed_at quedó persistido.
	var completed string
	if err := db.Get(&completed, `SELECT value FROM sync_state WHERE key = 'initial_sync_completed_at_ms'`); err != nil {
		t.Fatalf("initial_sync_completed_at no quedó persistido: %v", err)
	}
}

// TestPull_CadenaPartidaEntrePaginas — mismo edge case en el pull
// incremental (renovación hecha en OTRO device del gym).
func TestPull_CadenaPartidaEntrePaginas(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	memberID, typeID := seedMemberAndType(t, db, gymID)
	old, renewed, oldID, newID := renewalChain(t, gymID, memberID, typeID)

	cloud := &scriptedCloud{pullPages: map[string]syncpkg.PullResponse{
		"": {Changes: []syncpkg.PullChange{old}, HasMore: true,
			ServerNow: renewed.ServerUpdatedAt, SchemaVersion: syncpkg.SchemaVersion},
		old.ServerUpdatedAt.UTC().Format(time.RFC3339Nano): {
			Changes: []syncpkg.PullChange{renewed}, HasMore: false,
			ServerNow: renewed.ServerUpdatedAt, SchemaVersion: syncpkg.SchemaVersion},
	}}
	a := newScriptedAgent(t, cloud.serve(t), db, uow)

	if err := a.Pull(context.Background()); err != nil {
		t.Fatalf("Pull con cadena partida entre páginas: %v", err)
	}
	assertChainLanded(t, db, oldID, newID)
}

// TestFullSync_FKRotaReal_DiagnosticoNombraLaFila — cuando la FK está rota
// de verdad (replaced_by apunta a una membresía que no existe en TODO el
// feed), el full-sync debe fallar Y el error debe nombrar la fila culpable
// — nunca más un "FOREIGN KEY constraint failed" pelón.
func TestFullSync_FKRotaReal_DiagnosticoNombraLaFila(t *testing.T) {
	gymID := uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	memberID, typeID := seedMemberAndType(t, db, gymID)
	old, _, oldID, _ := renewalChain(t, gymID, memberID, typeID)

	serverNow := time.Date(2026, 7, 4, 12, 30, 0, 0, time.UTC)
	cloud := &scriptedCloud{fullPages: map[string]syncpkg.FullSyncResponse{
		// Sólo la vieja — su replaced_by queda huérfano para siempre.
		"": {Changes: []syncpkg.PullChange{old}, HasMore: false,
			ServerNow: serverNow, SchemaVersion: syncpkg.SchemaVersion},
	}}
	a := newScriptedAgent(t, cloud.serve(t), db, uow)

	err := a.FullSync(context.Background())
	if err == nil {
		t.Fatal("quería fallo por FK huérfana, got nil")
	}
	for _, frag := range []string{"FOREIGN KEY constraint failed", "memberships/" + oldID.String()} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error sin diagnóstico: %v (quería que contuviera %q)", err, frag)
		}
	}
}
