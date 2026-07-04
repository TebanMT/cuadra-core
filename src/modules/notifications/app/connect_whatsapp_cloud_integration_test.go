//go:build server && integration

package app_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	gymRepoPg "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notiWhatsApp "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/whatsapp"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// Integration del ownership cloud-authoritative de UC-037 (jul-2026): el
// connect de WhatsApp muta la tabla gyms SIN pasar por el push pipeline
// (el desktop llega vía WhatsAppSidecarProxy → estas rutas cloud), así que
// la propagación a los sidecars depende de dos piezas que este test
// verifica juntas contra Postgres real:
//
//  1. ConnectWhatsApp.SyncTouch (TouchGym) bumpea server_updated_at +
//     version del journal → la fila entra al pull incremental (ListSince).
//  2. gymCanonicalAugmentExpr inyecta whatsapp_business_phone /
//     whatsapp_connected_at / whatsapp_business_token_enc VIVOS sobre el
//     payload guardado — el payload viejo del push jamás trae estas llaves
//     (enqueueGym las omite by design).
//
// Y el espejo: Disconnect vuelve a bumpear y el pull siguiente trae nulls.
//
// Corre con: DATABASE_URL=... go test -tags "server integration" ./src/modules/notifications/app/
// Skipea si no hay Postgres alcanzable (misma convención que el resto).

type nopAuditRecorder struct{}

func (nopAuditRecorder) Record(_ context.Context, _ sharedDomain.Transaction, _ audit.Entry) error {
	return nil
}

func seedGymWithStaleJournalRow(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	gymID := uuid.New()
	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone,
		                  subscription_plan, subscription_status)
		VALUES (?, ?, 1, NOW(), NOW(), 'WA Ownership Test Gym', 'MX', 'America/Mexico_City',
		        'plus_monthly', 'active')`, gymID, gymID).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	// La fila del journal como la deja un push previo del sidecar: sin
	// llaves whatsapp (enqueueGym las omite) y con cursor ya consumido.
	payload := `{"id":"` + gymID.String() + `","gym_id":"` + gymID.String() + `","name":"WA Ownership Test Gym"}`
	if err := db.Exec(`
		INSERT INTO sync_entities (gym_id, entity_type, entity_id, version, payload, server_updated_at)
		VALUES (?, 'gyms', ?, 1, ?::jsonb, NOW() - INTERVAL '1 hour')`,
		gymID, gymID, payload).Error; err != nil {
		t.Fatalf("seed sync_entities: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM audit_log WHERE gym_id = ?`, gymID)
		db.Exec(`DELETE FROM sync_entities WHERE gym_id = ?`, gymID)
		db.Exec(`DELETE FROM gyms WHERE id = ?`, gymID)
	})
	return gymID
}

func pullGymChange(t *testing.T, uow sharedDomain.UnitOfWork, store *syncpkg.PostgresStore,
	gymID uuid.UUID, since time.Time) map[string]any {
	t.Helper()
	tx, err := uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	changes, _, err := store.ListSince(context.Background(), tx, gymID, since, 100)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	for i := range changes {
		if changes[i].EntityType == "gyms" && changes[i].EntityID == gymID.String() {
			var payload map[string]any
			if err := json.Unmarshal(changes[i].Payload, &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			return payload
		}
	}
	t.Fatalf("la fila del gym NO entró al pull incremental (cambios: %d)", len(changes))
	return nil
}

func TestIntegration_ConnectWhatsApp_BumpeaJournalYElPullTraeLosCamposVivos(t *testing.T) {
	db := testutil.OpenPostgres(t)
	gymID := seedGymWithStaleJournalRow(t, db)
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	syncStore := syncpkg.NewPostgresStore()
	gyms := gymRepoPg.NewGymPostgresRepository()

	connectUC := notiApp.NewConnectWhatsApp(gyms, notiWhatsApp.NewMockProvider(), uow, nopAuditRecorder{})
	connectUC.SyncTouch = syncStore

	beforeConnect := time.Now().UTC().Add(-time.Minute)
	out, err := connectUC.Execute(context.Background(), notiApp.ConnectWhatsAppInput{
		GymID:       gymID,
		ActorUserID: uuid.New(),
		Phone:       "+524421112233",
	})
	if err != nil {
		t.Fatalf("ConnectWhatsApp: %v", err)
	}
	if out.Phone != "+524421112233" {
		t.Fatalf("phone normalizado = %q", out.Phone)
	}

	// El pull incremental desde el cursor pre-connect trae la fila con el
	// estado de conexión VIVO (inyectado por la augmentation — el payload
	// guardado no tiene estas llaves).
	payload := pullGymChange(t, uow, syncStore, gymID, beforeConnect)
	if payload["whatsapp_business_phone"] != "+524421112233" {
		t.Errorf("whatsapp_business_phone = %v, want +524421112233", payload["whatsapp_business_phone"])
	}
	if _, ok := payload["whatsapp_connected_at"].(float64); !ok {
		t.Errorf("whatsapp_connected_at = %v (%T), want epoch ms numérico",
			payload["whatsapp_connected_at"], payload["whatsapp_connected_at"])
	}
	// token_enc es NULL by design en MVP; la llave debe venir (el apply del
	// sidecar la bind-ea) pero con JSON null.
	if v, ok := payload["whatsapp_business_token_enc"]; !ok || v != nil {
		t.Errorf("whatsapp_business_token_enc = %v (present=%v), want null presente", v, ok)
	}
	// El payload base del push previo se preserva — la augmentation
	// concatena, no re-serializa.
	if payload["name"] != "WA Ownership Test Gym" {
		t.Errorf("payload base perdido: name=%v", payload["name"])
	}

	// ── Disconnect: mismo pipeline, campos de regreso a null ─────────────
	beforeDisconnect := time.Now().UTC()
	disconnectUC := notiApp.NewDisconnectWhatsApp(gyms, uow, nopAuditRecorder{})
	disconnectUC.SyncTouch = syncStore
	if err := disconnectUC.Execute(context.Background(), notiApp.DisconnectWhatsAppInput{
		GymID:       gymID,
		ActorUserID: uuid.New(),
	}); err != nil {
		t.Fatalf("DisconnectWhatsApp: %v", err)
	}
	payload = pullGymChange(t, uow, syncStore, gymID, beforeDisconnect)
	if payload["whatsapp_business_phone"] != nil {
		t.Errorf("tras disconnect, whatsapp_business_phone = %v, want null", payload["whatsapp_business_phone"])
	}
	if payload["whatsapp_connected_at"] != nil {
		t.Errorf("tras disconnect, whatsapp_connected_at = %v, want null", payload["whatsapp_connected_at"])
	}
}
