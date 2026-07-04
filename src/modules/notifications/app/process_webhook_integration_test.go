//go:build sidecar

package app_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	eventDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/event"
	notification "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepoLite "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Body REAL de un StatusCallback de Twilio: application/x-www-form-urlencoded,
// NO JSON. Guardarlo crudo en whatsapp_events.raw_payload era el bug de prod
// jul-2026: postgres (JSONB) lo rechaza con 22P02, sqlite con el CHECK
// json_valid — el INSERT rollbackeaba TODO el webhook → 500 → Twilio
// reintentaba en loop. Este archivo reproduce ese caso contra el schema real.
const twilioStatusFormBody = "ChannelPrefix=whatsapp&ApiVersion=2010-04-01" +
	"&MessageStatus=sent&SmsSid=SM3f2c1e8a9b7d6f5e4a3b2c1d0e9f8a7b" +
	"&SmsStatus=sent&To=whatsapp%3A%2B5214421234567" +
	"&From=whatsapp%3A%2B14155238886" +
	"&MessageSid=SM3f2c1e8a9b7d6f5e4a3b2c1d0e9f8a7b" +
	"&AccountSid=ACa1b2c3d4e5f60718293a4b5c6d7e8f90"

func newWebhookUC(f *fixture) *notiApp.ProcessWebhook {
	return notiApp.NewProcessWebhook(
		f.notiRepo,
		notiRepoLite.NewWhatsAppEventSQLiteRepository(),
		f.uow,
	)
}

// rawPayloadFor lee raw_payload directo de la tabla para el assert de
// json_valid — el mismo dato que postgres validaría como JSONB.
func rawPayloadFor(f *fixture, providerMessageID string) string {
	f.t.Helper()
	var raw string
	if err := f.db.Get(&raw,
		`SELECT raw_payload FROM whatsapp_events WHERE provider_message_id = ?`,
		providerMessageID); err != nil {
		f.t.Fatalf("query whatsapp_events: %v", err)
	}
	return raw
}

// TestProcessWebhook_TwilioFormBody_GuardaJSONValido: repro del 22P02. Un
// callback cuyo RawPayload llega como form-urlencoded crudo NO debe tirar el
// INSERT — el evento se guarda con raw_payload JSON válido (el dominio
// garantiza el invariante envolviendo bytes no-JSON).
func TestProcessWebhook_TwilioFormBody_GuardaJSONValido(t *testing.T) {
	f := newFixture(t)
	uc := newWebhookUC(f)

	_, err := uc.Execute(context.Background(), notiApp.ProcessWebhookInput{
		EventType:         eventDomain.EventTypeStatus,
		ProviderMessageID: "SM3f2c1e8a9b7d6f5e4a3b2c1d0e9f8a7b",
		Status:            "sent",
		RawPayload:        []byte(twilioStatusFormBody),
	})
	if err != nil {
		t.Fatalf("Execute con body form-urlencoded debe pasar, falló: %v", err)
	}

	raw := rawPayloadFor(f, "SM3f2c1e8a9b7d6f5e4a3b2c1d0e9f8a7b")
	if !json.Valid([]byte(raw)) {
		t.Errorf("raw_payload guardado NO es JSON válido: %q", raw)
	}
}

// TestProcessWebhook_RawPayloadVacio_GuardaObjetoVacio: raw_payload es NOT
// NULL en ambos schemas — un payload vacío se persiste como "{}".
func TestProcessWebhook_RawPayloadVacio_GuardaObjetoVacio(t *testing.T) {
	f := newFixture(t)
	uc := newWebhookUC(f)

	_, err := uc.Execute(context.Background(), notiApp.ProcessWebhookInput{
		EventType:         eventDomain.EventTypeStatus,
		ProviderMessageID: "SMempty000000000000000000000000000",
		Status:            "queued",
		RawPayload:        nil,
	})
	if err != nil {
		t.Fatalf("Execute con payload vacío debe pasar, falló: %v", err)
	}
	raw := rawPayloadFor(f, "SMempty000000000000000000000000000")
	if raw != "{}" {
		t.Errorf("raw_payload = %q, want {}", raw)
	}
}

// createSentNotification inserta una notification en estado sent con el
// provider_message_id dado — el estado en que la deja el dispatcher después
// de un Send exitoso, listo para reconciliar callbacks.
func createSentNotification(f *fixture, providerMessageID string) uuid.UUID {
	f.t.Helper()
	now := time.Now().UTC()
	n, err := notification.New(
		uuid.New(), f.gymID, f.memberID,
		notification.ChannelWhatsApp, "expiry_reminder_3d", notification.RecipientMember,
		"+524429876543", map[string]string{}, now, now, nil,
	)
	if err != nil {
		f.t.Fatalf("notification.New: %v", err)
	}
	if err := n.MarkSent(providerMessageID, now); err != nil {
		f.t.Fatalf("MarkSent: %v", err)
	}
	if err := f.uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := f.notiRepo.Create(tx, n)
		return err
	}); err != nil {
		f.t.Fatalf("insert notification: %v", err)
	}
	return n.ID
}

func getNotification(f *fixture, id uuid.UUID) *notification.Notification {
	f.t.Helper()
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		f.t.Fatalf("query tx: %v", err)
	}
	n, err := f.notiRepo.GetByID(tx, id)
	if err != nil {
		f.t.Fatalf("get notification: %v", err)
	}
	return n
}

// TestProcessWebhook_StatusFailed_ReconciliaLaNotiSent: el dispatcher marca
// sent en cuanto Twilio ACEPTA el mensaje; el failed terminal de Meta llega
// después por webhook. La reconciliación cubre sent→failed: status corregido,
// sent_at limpiado (no cuenta como entregada en stats), razón del provider en
// error_message, y el evento ligado a la noti (gym_id + notification_id).
func TestProcessWebhook_StatusFailed_ReconciliaLaNotiSent(t *testing.T) {
	f := newFixture(t)
	uc := newWebhookUC(f)
	sid := "SMfailed00000000000000000000000000"
	notifID := createSentNotification(f, sid)

	out, err := uc.Execute(context.Background(), notiApp.ProcessWebhookInput{
		EventType:         eventDomain.EventTypeStatus,
		ProviderMessageID: sid,
		Status:            "failed",
		ErrorCode:         "63016",
		ErrorMessage:      "Failed to send freeform message",
		RawPayload:        []byte("MessageStatus=failed&MessageSid=" + sid + "&ErrorCode=63016"),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.NotificationID == nil || *out.NotificationID != notifID {
		t.Errorf("NotificationID = %v, want %v (evento ligado a la noti)", out.NotificationID, notifID)
	}
	if out.UpdatedStatus != notification.StatusFailed {
		t.Errorf("UpdatedStatus = %q, want failed", out.UpdatedStatus)
	}

	n := getNotification(f, notifID)
	if n.Status != notification.StatusFailed {
		t.Errorf("status = %q, want failed (sent→failed al llegar el terminal de Meta)", n.Status)
	}
	if n.SentAt != nil {
		t.Errorf("sent_at = %v, want nil (no entregada — no debe contar en stats de entregadas)", n.SentAt)
	}
	if n.FailedAt == nil {
		t.Errorf("failed_at debería estar seteado")
	}
	if n.ErrorMessage == nil || *n.ErrorMessage != "Failed to send freeform message" {
		t.Errorf("error_message = %v, want la razón del provider", n.ErrorMessage)
	}

	// El evento quedó registrado y ligado.
	events := notiRepoLite.NewWhatsAppEventSQLiteRepository()
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	evs, err := events.ListByNotification(tx, notifID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs) != 1 {
		t.Errorf("eventos ligados = %d, want 1", len(evs))
	} else {
		if evs[0].GymID == nil || *evs[0].GymID != f.gymID {
			t.Errorf("gym_id del evento = %v, want %v", evs[0].GymID, f.gymID)
		}
		if !json.Valid(evs[0].RawPayload) {
			t.Errorf("raw_payload del evento no es JSON válido: %q", evs[0].RawPayload)
		}
	}
}

// TestProcessWebhook_StatusUndelivered_ReconciliaConErrorCode: undelivered es
// el otro status terminal de fallo. Twilio suele mandarlo sin ErrorMessage —
// la razón cae al fallback "twilio: <status> (error <code>)" para que el
// operador tenga algo diagnosticable en el FE.
func TestProcessWebhook_StatusUndelivered_ReconciliaConErrorCode(t *testing.T) {
	f := newFixture(t)
	uc := newWebhookUC(f)
	sid := "SMundelivered000000000000000000000"
	notifID := createSentNotification(f, sid)

	_, err := uc.Execute(context.Background(), notiApp.ProcessWebhookInput{
		EventType:         eventDomain.EventTypeStatus,
		ProviderMessageID: sid,
		Status:            "undelivered",
		ErrorCode:         "63024",
		RawPayload:        []byte("MessageStatus=undelivered&MessageSid=" + sid + "&ErrorCode=63024"),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	n := getNotification(f, notifID)
	if n.Status != notification.StatusFailed {
		t.Errorf("status = %q, want failed", n.Status)
	}
	if n.ErrorMessage == nil || *n.ErrorMessage != "twilio: undelivered (error 63024)" {
		t.Errorf("error_message = %v, want fallback con error code", n.ErrorMessage)
	}
}

// TestProcessWebhook_StatusFailedDuplicado_EsIdempotente: Twilio reintenta
// callbacks (y puede duplicarlos). Un segundo failed sobre una noti ya failed
// sólo loguea el evento — la fila no se toca (misma version).
func TestProcessWebhook_StatusFailedDuplicado_EsIdempotente(t *testing.T) {
	f := newFixture(t)
	uc := newWebhookUC(f)
	sid := "SMdupfailed0000000000000000000000"
	notifID := createSentNotification(f, sid)

	in := notiApp.ProcessWebhookInput{
		EventType:         eventDomain.EventTypeStatus,
		ProviderMessageID: sid,
		Status:            "failed",
		ErrorMessage:      "Failed to send freeform message",
		RawPayload:        []byte("MessageStatus=failed&MessageSid=" + sid),
	}
	if _, err := uc.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute (1er callback): %v", err)
	}
	versionAfterFirst := getNotification(f, notifID).Version

	out, err := uc.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute (callback duplicado): %v", err)
	}
	if out.UpdatedStatus != "" {
		t.Errorf("UpdatedStatus = %q, want vacío (duplicado no reconcilia)", out.UpdatedStatus)
	}

	n := getNotification(f, notifID)
	if n.Status != notification.StatusFailed {
		t.Errorf("status = %q, want failed", n.Status)
	}
	if n.Version != versionAfterFirst {
		t.Errorf("version = %d, want %d (duplicado no debe tocar la fila)", n.Version, versionAfterFirst)
	}

	// Ambos eventos quedan logueados — el dedup es de la noti, no del log.
	events := notiRepoLite.NewWhatsAppEventSQLiteRepository()
	tx, err := f.uow.Query(context.Background())
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	evs, err := events.ListByNotification(tx, notifID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs) != 2 {
		t.Errorf("eventos logueados = %d, want 2", len(evs))
	}
}

// TestProcessWebhook_CallbackViejoTrasRetryManual_NoMataElReencolado: el
// retry manual re-arma la fila (failed→pending) y LIMPIA provider_message_id
// — un callback duplicado/tardío del SID del intento anterior ya no matchea
// la fila y no puede marcarla failed antes de que el dispatcher la despache.
// El evento igual queda logueado (sin ligar) para forense.
func TestProcessWebhook_CallbackViejoTrasRetryManual_NoMataElReencolado(t *testing.T) {
	f := newFixture(t)
	uc := newWebhookUC(f)
	sid := "SMretryrace0000000000000000000000"
	notifID := createSentNotification(f, sid)

	failedCallback := notiApp.ProcessWebhookInput{
		EventType:         eventDomain.EventTypeStatus,
		ProviderMessageID: sid,
		Status:            "failed",
		ErrorMessage:      "Failed to send freeform message",
		RawPayload:        []byte("MessageStatus=failed&MessageSid=" + sid),
	}
	if _, err := uc.Execute(context.Background(), failedCallback); err != nil {
		t.Fatalf("Execute (callback failed): %v", err)
	}

	// El dueño corrige la condición y reintenta — la fila vuelve a pending.
	retry := notiApp.NewRetryNotification(f.notiRepo, f.uow, audit.NewSQLiteRecorder())
	if err := retry.Execute(context.Background(), notiApp.RetryNotificationInput{
		GymID:          f.gymID,
		NotificationID: notifID,
		ActorUserID:    f.ownerID,
	}); err != nil {
		t.Fatalf("RetryNotification: %v", err)
	}
	n := getNotification(f, notifID)
	if n.Status != notification.StatusPending {
		t.Fatalf("tras retry: status = %q, want pending", n.Status)
	}
	if n.ProviderMessageID != nil {
		t.Fatalf("tras retry: provider_message_id = %q, want nil (el SID era del intento anterior)", *n.ProviderMessageID)
	}

	// Llega un callback tardío del SID viejo: no matchea la fila re-armada.
	out, err := uc.Execute(context.Background(), failedCallback)
	if err != nil {
		t.Fatalf("Execute (callback tardío): %v", err)
	}
	if out.NotificationID != nil {
		t.Errorf("NotificationID = %v, want nil (SID viejo ya no liga)", out.NotificationID)
	}
	n = getNotification(f, notifID)
	if n.Status != notification.StatusPending {
		t.Errorf("status = %q, want pending (el callback viejo no debe matar el reintento)", n.Status)
	}
}

// TestProcessWebhook_StatusDelivered_NoTocaLaNotiSent: delivered/read no
// cambian el status (sent_at ya está seteado por el dispatcher) — sólo se
// registra el evento para analytics. Documenta el comportamiento por diseño.
func TestProcessWebhook_StatusDelivered_NoTocaLaNotiSent(t *testing.T) {
	f := newFixture(t)
	uc := newWebhookUC(f)
	sid := "SMdelivered0000000000000000000000"
	notifID := createSentNotification(f, sid)

	_, err := uc.Execute(context.Background(), notiApp.ProcessWebhookInput{
		EventType:         eventDomain.EventTypeStatus,
		ProviderMessageID: sid,
		Status:            "delivered",
		RawPayload:        []byte("MessageStatus=delivered&MessageSid=" + sid),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	n := getNotification(f, notifID)
	if n.Status != notification.StatusSent {
		t.Errorf("status = %q, want sent (delivered sólo se loguea)", n.Status)
	}
}
