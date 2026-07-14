//go:build server && integration

package sync

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Round-trip del caso real que motivó rejected_duplicate: dos devices crean
// "Mensual" cada uno (o el sidecar crea un plan cuyo nombre ya existe
// cloud-side porque el journal no traía esa fila — p.ej. seed sin espejo).
// Contra Postgres REAL con las migraciones aplicadas, para que el 23505
// salga del índice de verdad (uq_membership_types_gym_name es sobre
// LOWER(name); un fake jamás capturaría eso) y atraviese GORM/pgx/UoW hasta
// processOne.
//
// Pinea las tres fases del flujo:
//  1. push del duplicado → rejected_duplicate con mensaje en español y el
//     valor del payload; la tx entera rollbackeó (ni domain ni journal).
//  2. el MISMO entity_id re-pusheado con el nombre corregido (lo que el
//     coalescing de sync_queue manda tras el rename en el desktop) → accepted.
//  3. ambas filas viven en la tabla de dominio.
//
// Run with -tags 'server integration'.
func TestPushDuplicate_RejectedLegibleAndRenameUnsticks(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	cases := []struct {
		entityType   string
		table        string
		buildPayload func(id uuid.UUID, name string) map[string]any
		dupFragment  string
		renamed      string
	}{
		{
			entityType: "membership_types",
			table:      "membership_types",
			buildPayload: func(id uuid.UUID, name string) map[string]any {
				return map[string]any{
					"id": id.String(), "gym_id": gymID.String(), "version": 1,
					"created_at": time.Now().UnixMilli(), "updated_at": time.Now().UnixMilli(),
					"name": name, "price": 500, "duration_days": 30,
					"enrollment_fee": 0, "maintenance_fee": 0, "active": true,
				}
			},
			dupFragment: `Ya existe un plan llamado "Mensual"`,
			renamed:     "Mensual recepción 2",
		},
		{
			entityType: "products",
			table:      "products",
			buildPayload: func(id uuid.UUID, name string) map[string]any {
				return map[string]any{
					"id": id.String(), "gym_id": gymID.String(), "version": 1,
					"created_at": time.Now().UnixMilli(), "updated_at": time.Now().UnixMilli(),
					"name": name, "price": 25, "stock": 0, "stock_minimum": 0, "active": true,
				}
			},
			dupFragment: `Ya existe un producto llamado "Mensual"`,
			renamed:     "Mensual botella",
		},
	}

	pushOne := func(t *testing.T, entityType string, entityID uuid.UUID, payload map[string]any) PushItemResult {
		t.Helper()
		pb, _ := json.Marshal(payload)
		req := PushRequest{
			ClientID:      uuid.NewString(),
			ClientNow:     time.Now(),
			SchemaVersion: 1,
			Batch: []PushItem{{
				QueueID:       uuid.NewString(),
				EntityType:    entityType,
				EntityID:      entityID.String(),
				Operation:     OpUpsertStr,
				ClientVersion: 1,
				Payload:       pb,
				EnqueuedAt:    time.Now(),
			}},
		}
		resp := doJSONReq(t, r, "POST", "/api/v1/sync/push", tok, req)
		if resp.Code != 200 {
			t.Fatalf("push HTTP %d: %s", resp.Code, resp.Body.String())
		}
		var pr PushResponse
		if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(pr.Results) != 1 {
			t.Fatalf("results: %+v", pr.Results)
		}
		return pr.Results[0]
	}

	for _, c := range cases {
		t.Run(c.entityType, func(t *testing.T) {
			deviceA := uuid.New()
			deviceB := uuid.New()

			// Device A crea "Mensual" — entra limpio.
			if res := pushOne(t, c.entityType, deviceA, c.buildPayload(deviceA, "Mensual")); res.Status != StatusAccepted {
				t.Fatalf("primer push: status=%s error=%q", res.Status, res.Error)
			}

			// Device B crea SU "Mensual" (id distinto, mismo nombre) —
			// choca con el unique index y debe salir legible y permanente.
			res := pushOne(t, c.entityType, deviceB, c.buildPayload(deviceB, "Mensual"))
			if res.Status != StatusRejectedDuplicate {
				t.Fatalf("duplicado: status=%s (error=%q), want %s", res.Status, res.Error, StatusRejectedDuplicate)
			}
			if !strings.Contains(res.Error, c.dupFragment) {
				t.Errorf("mensaje=%q, want fragmento %q", res.Error, c.dupFragment)
			}
			if strings.Contains(res.Error, "SQLSTATE") || strings.Contains(res.Error, "duplicate key value") {
				t.Errorf("el mensaje sigue crudo de Postgres: %q", res.Error)
			}

			// La tx del item rollbackeó completa: ni fila de dominio ni de
			// journal para deviceB (si el journal quedara, el pull
			// propagaría un registro que el dominio rechazó).
			var domainRows, journalRows int
			if err := db.Raw(`SELECT COUNT(*) FROM `+c.table+` WHERE gym_id = ? AND id = ?`,
				gymID, deviceB).Scan(&domainRows).Error; err != nil {
				t.Fatalf("count domain: %v", err)
			}
			if err := db.Raw(`SELECT COUNT(*) FROM sync_entities WHERE gym_id = ? AND entity_type = ? AND entity_id = ?`,
				gymID, c.entityType, deviceB).Scan(&journalRows).Error; err != nil {
				t.Fatalf("count journal: %v", err)
			}
			if domainRows != 0 || journalRows != 0 {
				t.Fatalf("el rechazo dejó residuos: domain=%d journal=%d, want 0/0", domainRows, journalRows)
			}

			// El operador renombra en el desktop → el coalescing re-encola el
			// MISMO entity_id con el nombre nuevo → el siguiente push entra.
			if res := pushOne(t, c.entityType, deviceB, c.buildPayload(deviceB, c.renamed)); res.Status != StatusAccepted {
				t.Fatalf("push renombrado: status=%s error=%q, want accepted", res.Status, res.Error)
			}

			var total int
			if err := db.Raw(`SELECT COUNT(*) FROM `+c.table+` WHERE gym_id = ? AND deleted_at IS NULL`,
				gymID).Scan(&total).Error; err != nil {
				t.Fatalf("count final: %v", err)
			}
			if total != 2 {
				t.Errorf("filas de dominio tras el rename = %d, want 2", total)
			}
		})
	}
}

// El índice es sobre LOWER(name): "mensual" vs "Mensual" TAMBIÉN choca. El
// mensaje debe salir igual de legible (el valor citado es el del payload
// entrante, que es el que el operador reconoce como suyo).
func TestPushDuplicate_CaseInsensitive(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	r, tokens := newRealHandler(t, db)
	tok, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	mk := func(id uuid.UUID, name string) []byte {
		b, _ := json.Marshal(map[string]any{
			"id": id.String(), "gym_id": gymID.String(), "version": 1,
			"created_at": time.Now().UnixMilli(), "updated_at": time.Now().UnixMilli(),
			"name": name, "price": 500, "duration_days": 30,
			"enrollment_fee": 0, "maintenance_fee": 0, "active": true,
		})
		return b
	}
	push := func(id uuid.UUID, payload []byte) PushItemResult {
		req := PushRequest{
			ClientID: uuid.NewString(), ClientNow: time.Now(), SchemaVersion: 1,
			Batch: []PushItem{{
				QueueID: uuid.NewString(), EntityType: "membership_types",
				EntityID: id.String(), Operation: OpUpsertStr, ClientVersion: 1,
				Payload: payload, EnqueuedAt: time.Now(),
			}},
		}
		resp := doJSONReq(t, r, "POST", "/api/v1/sync/push", tok, req)
		if resp.Code != 200 {
			t.Fatalf("push HTTP %d: %s", resp.Code, resp.Body.String())
		}
		var pr PushResponse
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		return pr.Results[0]
	}

	a, b := uuid.New(), uuid.New()
	if res := push(a, mk(a, "Mensual")); res.Status != StatusAccepted {
		t.Fatalf("primer push: %+v", res)
	}
	res := push(b, mk(b, "MENSUAL"))
	if res.Status != StatusRejectedDuplicate {
		t.Fatalf("status=%s error=%q, want rejected_duplicate", res.Status, res.Error)
	}
	if !strings.Contains(res.Error, `"MENSUAL"`) {
		t.Errorf("el mensaje debe citar el nombre TAL COMO lo escribió el operador: %q", res.Error)
	}
}
