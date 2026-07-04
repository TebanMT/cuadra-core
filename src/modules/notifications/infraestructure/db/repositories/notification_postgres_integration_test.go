//go:build server && integration

package repositories

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

// Suite de integración del mirror notification_queue → sync_entities (gap de
// propagación: los cambios de status cloud-side no llegaban al sidecar porque
// Update sólo tocaba la tabla de dominio). Run:
//
//	go test -tags 'server integration' ./src/modules/notifications/...

func notiTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.OpenPostgres(t)
}

func seedNotiGym(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	gymID := uuid.New()
	if err := db.Exec(`
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, country, timezone)
		VALUES (?, ?, 1, NOW(), NOW(), 'Noti Mirror Test Gym', 'MX', 'America/Mexico_City')`,
		gymID, gymID).Error; err != nil {
		t.Fatalf("seed gym: %v", err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"sync_entities", "notification_queue", "gyms"} {
			_ = db.Exec("DELETE FROM "+table+" WHERE gym_id = ?", gymID).Error
		}
	})
	return gymID
}

// newPendingNotification arma una notification cloud-side con el shape del
// scheduler de expiry reminders (idempotency_key incluido).
func newPendingNotification(t *testing.T, gymID uuid.UUID) *notiDomain.Notification {
	t.Helper()
	now := time.Now().UTC()
	idemp := "expiry_reminder_3d:" + uuid.NewString()
	n, err := notiDomain.New(
		uuid.New(), gymID, uuid.New(),
		notiDomain.ChannelWhatsApp, "expiry_reminder_3d", notiDomain.RecipientMember,
		"+524421234567", map[string]string{"member_first_name": "Rocky"},
		now, now, &idemp,
	)
	if err != nil {
		t.Fatalf("notification.New: %v", err)
	}
	return n
}

type syncMirrorRow struct {
	Version         int
	Payload         string
	ServerUpdatedAt time.Time
	DeletedAt       *time.Time
}

// readMirror lee la fila de sync_entities del mirror; ok=false si no existe.
func readMirror(t *testing.T, db *gorm.DB, gymID, notiID uuid.UUID) (syncMirrorRow, bool) {
	t.Helper()
	var rows []syncMirrorRow
	if err := db.Raw(`
		SELECT version, payload::text AS payload, server_updated_at, deleted_at
		FROM sync_entities
		WHERE gym_id = ? AND entity_type = 'notification_queue' AND entity_id = ?`,
		gymID, notiID,
	).Scan(&rows).Error; err != nil {
		t.Fatalf("read sync_entities: %v", err)
	}
	if len(rows) == 0 {
		return syncMirrorRow{}, false
	}
	return rows[0], true
}

func decodeMirrorPayload(t *testing.T, row syncMirrorRow) map[string]any {
	t.Helper()
	var pl map[string]any
	if err := json.Unmarshal([]byte(row.Payload), &pl); err != nil {
		t.Fatalf("payload no es JSON: %v", err)
	}
	return pl
}

// epochMs asserta que el campo llegó como número JSON (epoch-ms, el shape que
// ApplyPullChange aterriza en el SQLite del sidecar) y lo devuelve.
func epochMs(t *testing.T, pl map[string]any, field string) int64 {
	t.Helper()
	f, ok := pl[field].(float64)
	if !ok {
		t.Fatalf("%s no es number (epoch-ms): %T (%v)", field, pl[field], pl[field])
	}
	return int64(f)
}

// TestNotificationMirrorIntegration_CreateDaDeAltaEnSyncEntities — una fila
// creada cloud-side (expiry reminder del scheduler) debe nacer visible para
// el pull del sidecar, no sólo en la tabla de dominio.
func TestNotificationMirrorIntegration_CreateDaDeAltaEnSyncEntities(t *testing.T) {
	db := notiTestDB(t)
	gymID := seedNotiGym(t, db)
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	repo := NewNotificationPostgresRepository()

	n := newPendingNotification(t, gymID)
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := repo.Create(tx, n)
		return err
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	row, ok := readMirror(t, db, gymID, n.ID)
	if !ok {
		t.Fatal("Create no dio de alta la fila en sync_entities")
	}
	if row.Version != 1 {
		t.Errorf("sync_entities.version = %d, want 1", row.Version)
	}
	pl := decodeMirrorPayload(t, row)
	if pl["status"] != notiDomain.StatusPending {
		t.Errorf("payload.status = %v, want pending", pl["status"])
	}
	if got := epochMs(t, pl, "scheduled_for"); got != n.ScheduledFor.UTC().UnixMilli() {
		t.Errorf("payload.scheduled_for = %d, want %d", got, n.ScheduledFor.UTC().UnixMilli())
	}
	epochMs(t, pl, "created_at")
	// La columna payload de la noti viaja como objeto anidado (jsonb), no
	// como string doblemente serializado.
	nested, ok := pl["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload.payload no es objeto: %T", pl["payload"])
	}
	if nested["member_first_name"] != "Rocky" {
		t.Errorf("payload.payload.member_first_name = %v", nested["member_first_name"])
	}
}

// TestNotificationMirrorIntegration_MarkSentPropagaStatus — el MarkSent del
// dispatcher debe bumpear version + server_updated_at en sync_entities para
// que el pull incremental del sidecar lo recoja.
func TestNotificationMirrorIntegration_MarkSentPropagaStatus(t *testing.T) {
	db := notiTestDB(t)
	gymID := seedNotiGym(t, db)
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	repo := NewNotificationPostgresRepository()

	n := newPendingNotification(t, gymID)
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := repo.Create(tx, n)
		return err
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, _ := readMirror(t, db, gymID, n.ID)

	now := time.Now().UTC()
	if err := n.MarkSent("SMmirror0000000000000000000000000", now); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := repo.Update(tx, n)
		return err
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	row, ok := readMirror(t, db, gymID, n.ID)
	if !ok {
		t.Fatal("mirror desapareció tras Update")
	}
	if row.Version != 2 {
		t.Errorf("sync_entities.version = %d, want 2", row.Version)
	}
	if !row.ServerUpdatedAt.After(before.ServerUpdatedAt) {
		t.Errorf("server_updated_at no bumpeó: before=%v after=%v — el pull incremental no lo recogería",
			before.ServerUpdatedAt, row.ServerUpdatedAt)
	}
	pl := decodeMirrorPayload(t, row)
	if pl["status"] != notiDomain.StatusSent {
		t.Errorf("payload.status = %v, want sent", pl["status"])
	}
	if got := epochMs(t, pl, "sent_at"); got != now.UnixMilli() {
		t.Errorf("payload.sent_at = %d, want %d", got, now.UnixMilli())
	}
	if v, _ := pl["version"].(float64); int(v) != 2 {
		t.Errorf("payload.version = %v, want 2 (el sidecar LWW compara contra esto)", pl["version"])
	}
}

// TestNotificationMirrorIntegration_SentAFailedLimpiaSentAt — la
// reconciliación de fallos terminales de Twilio (sent→failed, jul-2026) debe
// llegar al sidecar con sent_at = null explícito, o el desktop seguiría
// contando la noti como entregada.
func TestNotificationMirrorIntegration_SentAFailedLimpiaSentAt(t *testing.T) {
	db := notiTestDB(t)
	gymID := seedNotiGym(t, db)
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	repo := NewNotificationPostgresRepository()

	n := newPendingNotification(t, gymID)
	now := time.Now().UTC()
	if err := n.MarkSent("SMreconcile00000000000000000000000", now); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := repo.Create(tx, n)
		return err
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !n.ReconcileDeliveryFailure("twilio: undelivered (error 63024)", now.Add(time.Second)) {
		t.Fatal("ReconcileDeliveryFailure devolvió false desde sent")
	}
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := repo.Update(tx, n)
		return err
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	row, _ := readMirror(t, db, gymID, n.ID)
	pl := decodeMirrorPayload(t, row)
	if pl["status"] != notiDomain.StatusFailed {
		t.Errorf("payload.status = %v, want failed", pl["status"])
	}
	if v, present := pl["sent_at"]; !present || v != nil {
		t.Errorf("payload.sent_at = %v, want null explícito (limpia el sent stale del sidecar)", v)
	}
	epochMs(t, pl, "failed_at")
	if pl["error_message"] != "twilio: undelivered (error 63024)" {
		t.Errorf("payload.error_message = %v", pl["error_message"])
	}
	if row.Version != 3 {
		t.Errorf("sync_entities.version = %d, want 3 (New=1, MarkSent=2, Reconcile=3)", row.Version)
	}
}

// TestNotificationMirrorIntegration_UpdateSinFilaSyncLaDaDeAlta — filas que
// nacieron cloud-side SIN mirror (legacy pre-fix, o el INSERT crudo de
// enqueueWelcomeRenotify en el projector) no existen en sync_entities; el
// primer Update debe darlas de alta vía ON CONFLICT, no fallar silencioso
// con un UPDATE de cero filas.
func TestNotificationMirrorIntegration_UpdateSinFilaSyncLaDaDeAlta(t *testing.T) {
	db := notiTestDB(t)
	gymID := seedNotiGym(t, db)
	uow := sharedDomain.NewPostgresUnitOfWork(db)
	repo := NewNotificationPostgresRepository()

	// INSERT crudo a la tabla de dominio — emula la fila legacy sin mirror.
	notiID := uuid.New()
	if err := db.Exec(`
		INSERT INTO notification_queue
		    (id, gym_id, version, created_at, updated_at, channel, template_key,
		     recipient_type, recipient_id, recipient_address, payload, status,
		     retry_count, scheduled_for)
		 VALUES (?, ?, 1, NOW(), NOW(), 'whatsapp', 'member_welcome_number',
		         'member', ?, '+524427654321', '{"member_number":"42"}'::jsonb,
		         'pending', 0, NOW())`,
		notiID, gymID, uuid.New()).Error; err != nil {
		t.Fatalf("seed legacy notification: %v", err)
	}
	if _, ok := readMirror(t, db, gymID, notiID); ok {
		t.Fatal("precondición rota: la fila legacy no debería tener mirror")
	}

	var n *notiDomain.Notification
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		loaded, err := repo.GetByID(tx, notiID)
		if err != nil {
			return err
		}
		if err := loaded.MarkSent("SMlegacy0000000000000000000000000", time.Now().UTC()); err != nil {
			return err
		}
		n = loaded
		_, err = repo.Update(tx, loaded)
		return err
	}); err != nil {
		t.Fatalf("Update legacy: %v", err)
	}

	row, ok := readMirror(t, db, gymID, notiID)
	if !ok {
		t.Fatal("Update sobre fila sin mirror no la dio de alta en sync_entities")
	}
	if row.Version != n.Version {
		t.Errorf("sync_entities.version = %d, want %d", row.Version, n.Version)
	}
	pl := decodeMirrorPayload(t, row)
	if pl["status"] != notiDomain.StatusSent {
		t.Errorf("payload.status = %v, want sent", pl["status"])
	}
	if pl["template_key"] != "member_welcome_number" {
		t.Errorf("payload.template_key = %v (el upsert debe llevar el payload completo)", pl["template_key"])
	}
}
