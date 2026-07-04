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
	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	subDB "github.com/cuadra/cuadra-core/src/modules/subscriptions/infraestructure/db"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// Integration del bug jul-2026: el webhook de Stripe activaba la suscripción
// en la tabla gyms pero NO tocaba sync_entities → la fila del gym nunca
// entraba al pull incremental (ListSince) y el desktop seguía mostrando el
// trial. Verifica el fix end-to-end contra Postgres real:
//
//  1. RecordEvent(activated) bumpea server_updated_at del gym en el journal.
//  2. El siguiente ListSince(since=antes-del-evento) incluye la fila.
//  3. El payload sale AUGMENTADO con el status vivo (gymCanonicalAugmentExpr
//     inyecta subscription_plan/status sobre el payload guardado) — por eso
//     el touch no necesita re-serializar nada.
//
// Corre con: DATABASE_URL=... go test -tags "server integration" ./src/modules/subscriptions/app/
// Skipea si no hay Postgres alcanzable (misma convención que el projector).

func integrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.OpenPostgres(t)
}

type noopAudit struct{}

func (noopAudit) Record(_ context.Context, _ sharedDomain.Transaction, _ audit.Entry) error {
	return nil
}

func seedTrialGymWithSyncRow(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	gymID := uuid.New()
	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone,
		                  subscription_plan, subscription_status)
		VALUES (?, ?, 1, NOW(), NOW(), 'Sync Touch Test Gym', 'MX', 'America/Mexico_City',
		        'trial', 'active')`, gymID, gymID).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	// La fila del journal como la deja un push previo del sidecar — con
	// server_updated_at viejo, ya consumido por el cursor del sidecar.
	payload := `{"id":"` + gymID.String() + `","gym_id":"` + gymID.String() + `","name":"Sync Touch Test Gym"}`
	if err := db.Exec(`
		INSERT INTO sync_entities (gym_id, entity_type, entity_id, version, payload, server_updated_at)
		VALUES (?, 'gyms', ?, 1, ?::jsonb, NOW() - INTERVAL '1 hour')`,
		gymID, gymID, payload).Error; err != nil {
		t.Fatalf("seed sync_entities: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM subscription_events WHERE gym_id = ?`, gymID)
		db.Exec(`DELETE FROM sync_entities WHERE gym_id = ?`, gymID)
		db.Exec(`DELETE FROM gyms WHERE id = ?`, gymID)
	})
	return gymID
}

func syncServerUpdatedAt(t *testing.T, db *gorm.DB, gymID uuid.UUID) time.Time {
	t.Helper()
	var ts time.Time
	if err := db.Raw(`
		SELECT server_updated_at FROM sync_entities
		 WHERE gym_id = ? AND entity_type = 'gyms' AND entity_id = ?`,
		gymID, gymID).Scan(&ts).Error; err != nil {
		t.Fatalf("read server_updated_at: %v", err)
	}
	return ts
}

func syncJournalVersion(t *testing.T, db *gorm.DB, gymID uuid.UUID) int {
	t.Helper()
	var v int
	if err := db.Raw(`
		SELECT version FROM sync_entities
		 WHERE gym_id = ? AND entity_type = 'gyms' AND entity_id = ?`,
		gymID, gymID).Scan(&v).Error; err != nil {
		t.Fatalf("read journal version: %v", err)
	}
	return v
}

func TestIntegration_RecordEventActivate_BumpeaSyncYEntraAlPullIncremental(t *testing.T) {
	db := integrationDB(t)
	gymID := seedTrialGymWithSyncRow(t, db)
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	syncStore := syncpkg.NewPostgresStore()

	before := syncServerUpdatedAt(t, db, gymID)
	verBefore := syncJournalVersion(t, db, gymID)

	uc := subApp.NewRecordEvent(
		subDB.NewEventPostgresRepository(),
		gymRepoPg.NewGymPostgresRepository(),
		uow,
		noopAudit{},
	)
	uc.SyncTouch = syncStore

	out, err := uc.Execute(context.Background(), subApp.RecordEventInput{
		GymID:      gymID,
		Provider:   subDomain.ProviderStripe,
		Type:       subDomain.EventActivated,
		ExternalID: "evt_it_" + gymID.String(),
		Plan:       "standard_monthly",
		OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if !out.Applied {
		t.Fatalf("expected Applied=true")
	}

	// 1. El journal se bumpeó — timestamp Y versión. La versión importa tanto
	// como el timestamp: el sidecar que hizo el último push queda con
	// local.version == journal.version (stampLocalSynced), y su
	// ApplyPullChange descarta por LWW cualquier cambio con versión <= local.
	// Un touch timestamp-only mete la fila al pull pero el sidecar la tira.
	after := syncServerUpdatedAt(t, db, gymID)
	if !after.After(before) {
		t.Fatalf("server_updated_at no se movió: before=%v after=%v", before, after)
	}
	verAfter := syncJournalVersion(t, db, gymID)
	if verAfter <= verBefore {
		t.Fatalf("la versión del journal no se bumpeó: before=%d after=%d — el sidecar que pusheó descartaría el touch", verBefore, verAfter)
	}

	// 2+3. El pull incremental desde el cursor pre-evento incluye la fila
	// del gym, con el status VIVO inyectado por la augmentation.
	tx, err := uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	changes, _, err := syncStore.ListSince(context.Background(), tx, gymID, before, 100)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	var gymChange *syncpkg.PullChange
	for i := range changes {
		if changes[i].EntityType == "gyms" && changes[i].EntityID == gymID.String() {
			gymChange = &changes[i]
			break
		}
	}
	if gymChange == nil {
		t.Fatalf("la fila del gym NO entró al pull incremental (cambios: %d)", len(changes))
	}
	if gymChange.Version != verAfter {
		t.Errorf("PullChange.Version = %d, want %d — es la versión contra la que el sidecar hace LWW", gymChange.Version, verAfter)
	}
	var payload map[string]any
	if err := json.Unmarshal(gymChange.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["subscription_status"] != "active" || payload["subscription_plan"] != "standard_monthly" {
		t.Errorf("payload augmentado incorrecto: status=%v plan=%v",
			payload["subscription_status"], payload["subscription_plan"])
	}
	// El resto del payload guardado se preserva (no se re-serializó).
	if payload["name"] != "Sync Touch Test Gym" {
		t.Errorf("el payload base se perdió: name=%v", payload["name"])
	}
}

// TestIntegration_TouchGym_JournalAdelantado_BumpeaEstricto: tras un conflict
// client-wins el journal queda ADELANTE del row vivo (UpsertOne escribe
// row.Version+1 en sync_entities pero el projector proyecta a gyms la versión
// del payload del cliente). Si TouchGym sólo igualara gyms.version
// (GREATEST sin +1), el touch sería no-op de versión y el sidecar que pusheó
// — stampeado con la versión del journal — volvería a descartar la fila.
// Pin: el bump debe ser ESTRICTO sobre la versión previa del journal.
func TestIntegration_TouchGym_JournalAdelantado_BumpeaEstricto(t *testing.T) {
	db := integrationDB(t)
	gymID := seedTrialGymWithSyncRow(t, db)
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	syncStore := syncpkg.NewPostgresStore()

	// Simula el estado post-conflict-client-wins: journal en 5, row vivo en 2.
	if err := db.Exec(`UPDATE gyms SET version = 2 WHERE id = ?`, gymID).Error; err != nil {
		t.Fatalf("set gym version: %v", err)
	}
	if err := db.Exec(`
		UPDATE sync_entities SET version = 5
		 WHERE gym_id = ? AND entity_type = 'gyms' AND entity_id = ?`,
		gymID, gymID).Error; err != nil {
		t.Fatalf("set journal version: %v", err)
	}

	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		return syncStore.TouchGym(context.Background(), tx, gymID)
	})
	if err != nil {
		t.Fatalf("TouchGym: %v", err)
	}
	if got := syncJournalVersion(t, db, gymID); got != 6 {
		t.Errorf("journal version = %d, want 6 (bump estricto sobre 5, no GREATEST plano con gyms.version=2)", got)
	}
}

// TestIntegration_TouchEntity_SinFilaNoInserta: un gym cuyo sidecar nunca ha
// pusheado no tiene fila en el journal — el touch NO debe fabricar una (un
// payload parcial arriesga pisar campos al aplicarse en el sidecar). Devuelve
// touched=false y no inserta nada; el bootstrap/full sync cubre esos gyms.
func TestIntegration_TouchEntity_SinFilaNoInserta(t *testing.T) {
	db := integrationDB(t)
	gymID := uuid.New() // sin seed: no existe ni el gym ni la fila del journal
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	syncStore := syncpkg.NewPostgresStore()

	err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		touched, err := syncStore.TouchEntity(context.Background(), tx, gymID, "gyms", gymID)
		if err != nil {
			return err
		}
		if touched {
			t.Errorf("touched=true sin fila en el journal")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("TouchEntity: %v", err)
	}
	var count int64
	if err := db.Raw(`SELECT COUNT(*) FROM sync_entities WHERE gym_id = ?`, gymID).Scan(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("el touch insertó filas: %d", count)
	}
}
