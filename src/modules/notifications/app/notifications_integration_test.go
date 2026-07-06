//go:build sidecar

package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	billingApp "github.com/cuadra/cuadra-core/src/modules/billing/app"
	folioSvc "github.com/cuadra/cuadra-core/src/modules/billing/domain/folio"
	billingRepoLite "github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/repositories"
	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memRepoLite "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/repositories"
	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	notification "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepoLite "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/whatsapp"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

type fixture struct {
	t          *testing.T
	db         *sqlx.DB
	uow        sharedDomain.UnitOfWork
	gymRepo    *gymRepoLite.GymSQLiteRepository
	memberRepo *memRepoLite.MemberSQLiteRepository
	notiRepo   *notiRepoLite.NotificationSQLiteRepository
	gymID      uuid.UUID
	ownerID    uuid.UUID
	memberID   uuid.UUID
	planID     uuid.UUID
	registerUC *billingApp.RegisterMembershipPayment
	connectUC  *notiApp.ConnectWhatsApp
	provider   *whatsapp.MockProvider
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	dsn := dbPath + "?_foreign_keys=on"
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)

	// Todas las migraciones en orden — no un subset cherry-picked. Así el
	// schema del test matchea producción y no se rompe cada vez que una
	// migración agrega una columna a una tabla base. os.ReadDir ordena por
	// nombre y los archivos están zero-padded (001_..) → orden correcto.
	migDir := "../../../../db_migrations/sqlite"
	migEntries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range migEntries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		mig := filepath.Join(migDir, e.Name())
		schema, err := os.ReadFile(mig)
		if err != nil {
			t.Fatalf("read migration %s: %v", mig, err)
		}
		if _, err := db.Exec(string(schema)); err != nil {
			t.Fatalf("apply %s: %v", mig, err)
		}
	}

	uow := sharedDomain.NewSQLiteUnitOfWork(db, syncpkg.NewSqliteQueue())
	recorder := audit.NewSQLiteRecorder()

	signup := usersApp.NewSignupOwner(
		usersRepoLite.NewUserSQLiteRepository(),
		gymRepoLite.NewGymSQLiteRepository(),
		uow,
		auth.NewJWTService("test-secret"),
		recorder,
		30,
	)
	owner, err := signup.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Owner",
		Email:           "owner@gym.com",
		Password:        "supersecret123",
		PasswordConfirm: "supersecret123",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}

	gymRepo := gymRepoLite.NewGymSQLiteRepository()
	updateBasic := func() {
		// give the gym a name so the receipt template renders cleanly
		ctx := context.Background()
		if err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
			g, err := gymRepo.GetByID(tx, owner.GymID)
			if err != nil {
				return err
			}
			if err := g.UpdateBasicInfo("Gym Bros", "Querétaro"); err != nil {
				return err
			}
			// WhatsApp se conecta vía ProfileUpdate (post-Plus en producción),
			// pero el test de envío de notifications necesita un número
			// configurado en el gym para usar SendTemplate.
			wa := "+524421234567"
			if err := g.ApplyProfileUpdate(gymDomain.ProfileUpdate{WhatsApp: &wa}); err != nil {
				return err
			}
			_, err = gymRepo.Update(tx, g)
			return err
		}); err != nil {
			t.Fatalf("update basic: %v", err)
		}
	}
	updateBasic()

	mtRepo := memRepoLite.NewMembershipTypeSQLiteRepository()
	memberRepo := memRepoLite.NewMemberSQLiteRepository()
	membershipRepo := memRepoLite.NewMembershipSQLiteRepository()
	paymentRepo := billingRepoLite.NewPaymentSQLiteRepository()
	notificationRepo := notiRepoLite.NewNotificationSQLiteRepository()

	memberSvc := memApp.NewMemberService(memberRepo, membershipRepo, mtRepo)

	createMT := memApp.NewCreateMembershipType(mtRepo, uow, recorder)
	mt, err := createMT.Execute(context.Background(), memApp.CreateMembershipTypeInput{
		GymID: owner.GymID, ActorUserID: owner.UserID,
		Name: "Mensual", Price: 500, DurationDays: 30,
	})
	if err != nil {
		t.Fatalf("create mt: %v", err)
	}

	createMember := memApp.NewCreateMember(memberRepo, membershipRepo, mtRepo, uow, recorder)
	mem, err := createMember.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: owner.GymID, ActorUserID: owner.UserID,
		FullName: "Juan Pérez", Phone: "+524429876543",
		MembershipTypeID: mt.ID, StartDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	folios := folioSvc.NewGenerator(paymentRepo)
	enqueueReceipt := notiApp.NewEnqueueReceipt(
		notificationRepo, gymRepo, memberRepo,
		notiRepoLite.NewTemplateOverrideSQLiteRepository(), uow,
	)
	subscriber := notiApp.NewBillingEventSubscriber(enqueueReceipt)
	registerUC := billingApp.NewRegisterMembershipPayment(
		paymentRepo, folios, memberSvc, memberRepo, uow, recorder, subscriber,
	)

	provider := whatsapp.NewMockProvider()
	connectUC := notiApp.NewConnectWhatsApp(gymRepo, provider, uow, recorder)

	return &fixture{
		t:          t,
		db:         db,
		uow:        uow,
		gymRepo:    gymRepo,
		memberRepo: memberRepo,
		notiRepo:   notificationRepo,
		gymID:      owner.GymID,
		ownerID:    owner.UserID,
		memberID:   mem.MemberID,
		planID:     mt.ID,
		registerUC: registerUC,
		connectUC:  connectUC,
		provider:   provider,
	}
}

// ---------------------------------------------------------------------------
// UC-037 — Connect WhatsApp
// ---------------------------------------------------------------------------

func TestUC037_ConnectWhatsApp_StoresPhoneAndConnectedAt(t *testing.T) {
	f := newFixture(t)
	out, err := f.connectUC.Execute(context.Background(), notiApp.ConnectWhatsAppInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Phone: "+524421112233",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if out.Phone != "+524421112233" || out.SenderID == "" {
		t.Errorf("output: %+v", out)
	}
	if out.ConnectedAt.IsZero() {
		t.Errorf("connected_at empty")
	}

	// Persisted on the gym row.
	tx, _ := f.uow.Query(context.Background())
	g, err := f.gymRepo.GetByID(tx, f.gymID)
	if err != nil {
		t.Fatalf("get gym: %v", err)
	}
	if !g.IsWhatsAppConnected() {
		t.Errorf("expected connected")
	}
}

func TestUC037_ConnectWhatsApp_RejectsInvalidPhone(t *testing.T) {
	f := newFixture(t)
	_, err := f.connectUC.Execute(context.Background(), notiApp.ConnectWhatsAppInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Phone: "abc",
	})
	if err == nil {
		t.Fatal("expected error for invalid phone")
	}
}

// ---------------------------------------------------------------------------
// UC-039 — Comprobante automático (cross-BC: PaymentCompleted -> receipt)
// ---------------------------------------------------------------------------

func TestUC039_PaymentCompletedEnqueuesReceiptWhenWhatsAppConnected(t *testing.T) {
	f := newFixture(t)

	// Connect WhatsApp first.
	if _, err := f.connectUC.Execute(context.Background(), notiApp.ConnectWhatsAppInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Phone: "+524421112233",
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Charge the member.
	if _, err := f.registerUC.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash",
	}); err != nil {
		t.Fatalf("register payment: %v", err)
	}

	// Notification queue should now have one row.
	tx, _ := f.uow.Query(context.Background())
	rows, total, err := f.notiRepo.ListByGym(tx, f.gymID, "", 1, 50)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("expected 1 notification, got %d (rows=%d)", total, len(rows))
	}
	n := rows[0]
	if n.TemplateKey != "receipt_membership" {
		t.Errorf("template_key = %s, want receipt_membership", n.TemplateKey)
	}
	if n.Channel != notification.ChannelWhatsApp {
		t.Errorf("channel = %s", n.Channel)
	}
	if n.Status != "pending" {
		t.Errorf("status = %s, want pending", n.Status)
	}
	if n.RecipientAddress != "+524429876543" {
		t.Errorf("recipient = %s", n.RecipientAddress)
	}
	if n.IdempotencyKey == nil {
		t.Errorf("idempotency_key should be set on receipt")
	}
}

// ADR-009: un gym sin número propio conectado (Standard/trial) ya NO hace
// skip — el recibo se encola igual y el dispatcher lo manda desde el número
// maestro de Tinta. El gate viejo (skip si no conectado) quedó obsoleto.
func TestUC039_PaymentCompleted_EnqueuesEvenWhenNotConnected(t *testing.T) {
	f := newFixture(t)
	// Skip the connect step on purpose — Tinta es el sender en este tier.

	if _, err := f.registerUC.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash",
	}); err != nil {
		t.Fatalf("register payment: %v", err)
	}

	tx, _ := f.uow.Query(context.Background())
	_, total, err := f.notiRepo.ListByGym(tx, f.gymID, "", 1, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Errorf("expected 1 receipt enqueued via Tinta master sender, got %d", total)
	}
}

// ---------------------------------------------------------------------------
// Dispatcher — happy path with mock provider returning success
// ---------------------------------------------------------------------------

func TestDispatcher_PendingRowSentByMockProvider(t *testing.T) {
	f := newFixture(t)
	if _, err := f.connectUC.Execute(context.Background(), notiApp.ConnectWhatsAppInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Phone: "+524421112233",
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := f.registerUC.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash",
	}); err != nil {
		t.Fatalf("register payment: %v", err)
	}

	dispatcher := notiApp.NewDispatchNotification(
		f.notiRepo,
		notiRepoLite.NewTemplateOverrideSQLiteRepository(),
		f.gymRepo,
		f.provider,
		nil,
		f.uow,
	)
	// Los templates de recibo requieren el renderer del PDF + el uploader de
	// R2 para inyectar {receipt_url}; en el test usamos fakes.
	dispatcher.Receipt = fakeReceiptRenderer{}
	dispatcher.Media = fakeMediaUploader{}
	sent, err := dispatcher.Tick(context.Background(), time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if sent != 1 {
		t.Errorf("expected 1 sent, got %d", sent)
	}
	if f.provider.SentCount() != 1 {
		t.Errorf("provider captured %d sends", f.provider.SentCount())
	}

	// Row should now be `sent` with a provider message id.
	tx, _ := f.uow.Query(context.Background())
	rows, _, _ := f.notiRepo.ListByGym(tx, f.gymID, "sent", 1, 50)
	if len(rows) != 1 {
		t.Fatalf("expected 1 sent row, got %d", len(rows))
	}
	if rows[0].ProviderMessageID == nil || *rows[0].ProviderMessageID == "" {
		t.Errorf("provider_message_id missing")
	}
	if rows[0].SentAt == nil {
		t.Errorf("sent_at missing")
	}
}

func TestDispatcher_TransientFailureRetries(t *testing.T) {
	f := newFixture(t)
	if _, err := f.connectUC.Execute(context.Background(), notiApp.ConnectWhatsAppInput{
		GymID: f.gymID, ActorUserID: f.ownerID, Phone: "+524421112233",
	}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := f.registerUC.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID,
		Method: "cash",
	}); err != nil {
		t.Fatalf("register payment: %v", err)
	}

	f.provider.FailNext = 1
	dispatcher := notiApp.NewDispatchNotification(
		f.notiRepo,
		notiRepoLite.NewTemplateOverrideSQLiteRepository(),
		f.gymRepo,
		f.provider,
		nil,
		f.uow,
	)
	// Los templates de recibo requieren el renderer del PDF + el uploader de
	// R2 para inyectar {receipt_url}; en el test usamos fakes.
	dispatcher.Receipt = fakeReceiptRenderer{}
	dispatcher.Media = fakeMediaUploader{}
	sent, err := dispatcher.Tick(context.Background(), time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if sent != 0 {
		t.Errorf("expected 0 sent on first transient failure, got %d", sent)
	}

	// Retry — provider succeeds the second time. Tick claim window is
	// 5 minutes, so we advance 6 minutes.
	sent, err = dispatcher.Tick(context.Background(), time.Now().UTC().Add(6*time.Minute))
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if sent != 1 {
		t.Errorf("expected 1 sent on retry, got %d", sent)
	}
}

// Nota: la supresión de mensajes stale (stale → `held`) NO se testea sobre este
// harness SQLite porque `held` es un estado SÓLO de Postgres (el dispatcher
// corre únicamente en el cloud y `held` nunca sincroniza al sidecar). El test
// DB-agnóstico de la decisión del dispatcher vive en stale_dispatch_test.go.

// ---------------------------------------------------------------------------
// Fakes para el flujo de recibo (PDF + R2): los templates receipt_* requieren
// un ReceiptPDFRenderer + un PublicMediaUploader para inyectar {receipt_url}.
// ---------------------------------------------------------------------------

type fakeReceiptRenderer struct{}

func (fakeReceiptRenderer) RenderReceiptPDF(_ context.Context, _, _ uuid.UUID) ([]byte, string, error) {
	return []byte("%PDF-1.4 fake"), "application/pdf", nil
}

type fakeMediaUploader struct{}

func (fakeMediaUploader) UploadPublic(_ context.Context, objectKey, _ string, _ []byte) (string, error) {
	return "https://media.test/" + objectKey, nil
}

// El recibo de membresía debe llevar el TIPO real ("Membresía <plan>"), la
// vigencia real (no el approx hoy+30d con literal "ene") y el payment_id que
// el dispatcher usa para generar el PDF. Regresión de los bugs de datos.
func TestEnqueueReceipt_RealTypeExpiryAndPaymentID(t *testing.T) {
	f := newFixture(t)
	if _, err := f.registerUC.Execute(context.Background(), billingApp.RegisterMembershipPaymentInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		MemberID: f.memberID, MembershipTypeID: f.planID, Method: "cash",
	}); err != nil {
		t.Fatalf("register payment: %v", err)
	}

	tx, _ := f.uow.Query(context.Background())
	rows, _, err := f.notiRepo.ListByGym(tx, f.gymID, "pending", 1, 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var receipt *notification.Notification
	for _, r := range rows {
		if r.TemplateKey == "receipt_membership" {
			receipt = r
			break
		}
	}
	if receipt == nil {
		t.Fatal("no se encoló el recibo de membresía")
	}
	// El plan del fixture se llama "Mensual" → tipo = "Membresía Mensual"
	// (no el hardcode anterior "Mensual" a secas).
	if got := receipt.Payload["membership_type"]; got != "Membresía Mensual" {
		t.Errorf("membership_type = %q, want %q", got, "Membresía Mensual")
	}
	// payment_id presente y parseable (lo usa el dispatcher para el PDF).
	if _, err := uuid.Parse(receipt.Payload["payment_id"]); err != nil {
		t.Errorf("payment_id inválido/ausente: %q", receipt.Payload["payment_id"])
	}
	// Vigencia real, no vacía y con formato "DD mmm YYYY".
	if exp := receipt.Payload["expiry_date"]; len(exp) < 8 {
		t.Errorf("expiry_date no parece una fecha real: %q", exp)
	}
}
