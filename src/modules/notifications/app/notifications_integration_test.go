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
	t           *testing.T
	db          *sqlx.DB
	uow         sharedDomain.UnitOfWork
	gymRepo     *gymRepoLite.GymSQLiteRepository
	memberRepo  *memRepoLite.MemberSQLiteRepository
	notiRepo    *notiRepoLite.NotificationSQLiteRepository
	gymID       uuid.UUID
	ownerID     uuid.UUID
	memberID    uuid.UUID
	planID      uuid.UUID
	registerUC  *billingApp.RegisterMembershipPayment
	connectUC   *notiApp.ConnectWhatsApp
	provider    *whatsapp.MockProvider
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

	for _, mig := range []string{
		"../../../../db_migrations/sqlite/001_init_schema.sql",
		"../../../../db_migrations/sqlite/002_notifications.sql",
	} {
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
			if err := g.UpdateBasicInfo("Gym Bros", "Querétaro", "+524421234567"); err != nil {
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
	enqueueReceipt := notiApp.NewEnqueueReceipt(notificationRepo, gymRepo, memberRepo, uow)
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

func TestUC039_PaymentCompleted_SkipsWhenWhatsAppNotConnected(t *testing.T) {
	f := newFixture(t)
	// Skip the connect step on purpose.

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
	if total != 0 {
		t.Errorf("expected 0 notifications when whatsapp not connected, got %d", total)
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
