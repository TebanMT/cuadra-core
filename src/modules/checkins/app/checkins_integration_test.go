//go:build sidecar && bio_mock

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

	chkApp "github.com/cuadra/cuadra-core/src/modules/checkins/app"
	chkRepoLite "github.com/cuadra/cuadra-core/src/modules/checkins/infraestructure/db/repositories"
	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
	memRepoLite "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

// fixture wires a fresh SQLite DB with schema applied + an owner+gym signed
// up + one membership type + one member with an active membership. Returns
// everything the checkins use cases need.
type checkinsFixture struct {
	t        *testing.T
	uow      sharedDomain.UnitOfWork
	recorder audit.Recorder
	gymID    uuid.UUID
	ownerID  uuid.UUID
	memberID uuid.UUID

	memberRepo      *memRepoLite.MemberSQLiteRepository
	fingerprintRepo *memRepoLite.FingerprintSQLiteRepository
	checkinRepo     *chkRepoLite.CheckinSQLiteRepository
	memberSvc       *memApp.MemberService

	gmkProvider *bcrypto.InMemoryGMKProvider
	reader      *biometric.MockReader
}

func setupCheckinsFixture(t *testing.T) *checkinsFixture {
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

	for _, m := range []string{
		"../../../../db_migrations/sqlite/001_init_schema.sql",
		"../../../../db_migrations/sqlite/005_users_pin.sql",
		"../../../../db_migrations/sqlite/008_gym_charge_settings.sql",
	} {
		schema, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(schema)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
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
	out, err := signup.Execute(context.Background(), usersApp.SignupOwnerInput{
		FullName:        "Owner",
		Email:           "owner@gym.com",
		Password:        "supersecret123",
		PasswordConfirm: "supersecret123",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}

	mtRepo := memRepoLite.NewMembershipTypeSQLiteRepository()
	memberRepo := memRepoLite.NewMemberSQLiteRepository()
	membershipRepo := memRepoLite.NewMembershipSQLiteRepository()
	fpRepo := memRepoLite.NewFingerprintSQLiteRepository()
	checkinRepo := chkRepoLite.NewCheckinSQLiteRepository()

	memberSvc := memApp.NewMemberService(memberRepo, membershipRepo, mtRepo).WithFingerprints(fpRepo)

	createMT := memApp.NewCreateMembershipType(mtRepo, uow, recorder)
	mtOut, err := createMT.Execute(context.Background(), memApp.CreateMembershipTypeInput{
		GymID: out.GymID, ActorUserID: out.UserID,
		Name: "Mensual", Price: 500, DurationDays: 30,
	})
	if err != nil {
		t.Fatalf("createMT: %v", err)
	}

	createMember := memApp.NewCreateMember(memberRepo, membershipRepo, mtRepo, uow, recorder)
	mOut, err := createMember.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: out.GymID, ActorUserID: out.UserID,
		FullName: "Juan Pérez", Phone: "5551234567",
		MembershipTypeID: mtOut.ID, StartDate: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("createMember: %v", err)
	}
	// El socio nace en pending_payment (sin ChargeFirstPayment). Para
	// estos tests necesitamos la membresía activa — la activamos vía
	// el seam de members.RenewMembershipForPayment, que detecta el
	// estado pending y llama Membership.Activate() en lugar de renovar.
	if err := uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := memberSvc.RenewMembershipForPayment(context.Background(), tx, memApp.RenewMembershipForPaymentInput{
			MemberID: mOut.MemberID, MembershipTypeID: mtOut.ID, PaymentDate: time.Now().UTC(),
		}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatalf("activate membership: %v", err)
	}

	gmkProvider := bcrypto.NewInMemoryGMKProvider()
	gmkProvider.SetDeterministic(out.GymID, "test-gmk-seed")

	reader := biometric.NewMockReader()
	reader.GMK = gmkProvider
	reader.GymID = out.GymID

	return &checkinsFixture{
		t:               t,
		uow:             uow,
		recorder:        recorder,
		gymID:           out.GymID,
		ownerID:         out.UserID,
		memberID:        mOut.MemberID,
		memberRepo:      memberRepo,
		fingerprintRepo: fpRepo,
		checkinRepo:     checkinRepo,
		memberSvc:       memberSvc,
		gmkProvider:     gmkProvider,
		reader:          reader,
	}
}

// TestUC028AndUC029_FingerprintEnrollmentAndCheckin walks the full flow:
// register a fingerprint (UC-028), then check in via the mock reader (UC-029).
// Verifies that the matched checkin row has the expected method, result, and
// no operator (auto checkin).
func TestUC028AndUC029_FingerprintEnrollmentAndCheckin(t *testing.T) {
	f := setupCheckinsFixture(t)
	ctx := context.Background()

	// UC-028: register a fingerprint with a deterministic plaintext template.
	templatePlain := []byte("template-bytes-juan-perez-0001")
	registerFP := memApp.NewRegisterFingerprint(f.memberRepo, f.fingerprintRepo, f.gmkProvider, f.uow, f.recorder)
	regOut, err := registerFP.Execute(ctx, memApp.RegisterFingerprintInput{
		GymID:           f.gymID,
		ActorUserID:     f.ownerID,
		MemberID:        f.memberID,
		ConsentAccepted: true,
		Capture: &biometric.CaptureResult{
			Bytes:        append([]byte{}, templatePlain...), // copy — register zeroes the input
			Format:       biometric.CaptureResult{}.Format,
			QualityScore: 92,
		},
	})
	if err != nil {
		t.Fatalf("RegisterFingerprint: %v", err)
	}
	if regOut.MemberID != f.memberID {
		t.Errorf("RegisterFingerprint memberID mismatch")
	}

	// Re-registering should be rejected (DA-28.2 — 1 huella por socio en MVP).
	_, err = registerFP.Execute(ctx, memApp.RegisterFingerprintInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		ConsentAccepted: true,
		Capture: &biometric.CaptureResult{
			Bytes: append([]byte{}, templatePlain...), QualityScore: 92,
		},
	})
	if err == nil {
		t.Errorf("expected error on second enrollment")
	}

	// UC-029: stage the same plaintext as the next capture, then run the
	// fingerprint use case. Mock matches by exact plaintext equality.
	checkin := chkApp.NewCheckinByFingerprint(f.memberSvc, f.checkinRepo, f.reader, f.uow, f.recorder)
	f.reader.QueueCapture(&biometric.CaptureResult{
		Bytes:        append([]byte{}, templatePlain...),
		QualityScore: 88,
	})
	view, err := checkin.Execute(ctx, chkApp.CheckinByFingerprintInput{
		GymID:   f.gymID,
		Capture: &biometric.CaptureResult{Bytes: append([]byte{}, templatePlain...), QualityScore: 90},
	})
	if err != nil {
		t.Fatalf("CheckinByFingerprint: %v", err)
	}
	if view.MemberID != f.memberID {
		t.Errorf("checkin matched wrong member: %v vs %v", view.MemberID, f.memberID)
	}
	if view.Method != "fingerprint" {
		t.Errorf("expected method=fingerprint, got %s", view.Method)
	}
	// New member with fresh 30-day membership → AllowedActive (>7 days).
	if view.AccessStatus != access.AllowedActive {
		t.Errorf("expected AllowedActive, got %s", view.AccessStatus)
	}
	if view.Result != "allowed_active" {
		t.Errorf("expected result allowed_active, got %s", view.Result)
	}

	// Checkin should be persisted.
	tx, err := f.uow.Query(ctx)
	if err != nil {
		t.Fatalf("query tx: %v", err)
	}
	got, err := f.checkinRepo.GetByID(tx, view.CheckinID)
	if err != nil {
		t.Fatalf("get checkin: %v", err)
	}
	if got.MemberID != f.memberID || got.Method != "fingerprint" {
		t.Errorf("persisted checkin mismatch: %+v", got)
	}
}

// TestUC029_NoMatch_ReturnsBusinessError verifies that a capture that doesn't
// match any enrolled template fails cleanly.
func TestUC029_NoMatch_ReturnsBusinessError(t *testing.T) {
	f := setupCheckinsFixture(t)
	ctx := context.Background()

	// Enroll a fingerprint.
	registerFP := memApp.NewRegisterFingerprint(f.memberRepo, f.fingerprintRepo, f.gmkProvider, f.uow, f.recorder)
	if _, err := registerFP.Execute(ctx, memApp.RegisterFingerprintInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		ConsentAccepted: true,
		Capture:         &biometric.CaptureResult{Bytes: []byte("template-A"), QualityScore: 85},
	}); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// Try a different plaintext.
	checkin := chkApp.NewCheckinByFingerprint(f.memberSvc, f.checkinRepo, f.reader, f.uow, f.recorder)
	_, err := checkin.Execute(ctx, chkApp.CheckinByFingerprintInput{
		GymID:   f.gymID,
		Capture: &biometric.CaptureResult{Bytes: []byte("template-DIFFERENT"), QualityScore: 90},
	})
	if err == nil {
		t.Fatalf("expected ErrNoFingerprintMatch (wrapped)")
	}
}

// TestUC030_ManualCheckin_Allowed records a manual checkin for a member with
// an active membership. Verifies the operator is captured.
func TestUC030_ManualCheckin_Allowed(t *testing.T) {
	f := setupCheckinsFixture(t)
	ctx := context.Background()

	uc := chkApp.NewCheckinManual(f.memberSvc, f.checkinRepo, f.uow, f.recorder)
	view, err := uc.Execute(ctx, chkApp.CheckinManualInput{
		GymID:      f.gymID,
		OperatorID: f.ownerID,
		MemberID:   f.memberID,
	})
	if err != nil {
		t.Fatalf("manual: %v", err)
	}
	if view.Method != "manual" {
		t.Errorf("method should be manual, got %s", view.Method)
	}
	if view.Result != "allowed_active" {
		t.Errorf("expected allowed_active, got %s", view.Result)
	}

	tx, _ := f.uow.Query(ctx)
	got, _ := f.checkinRepo.GetByID(tx, view.CheckinID)
	if got.OperatorID == nil || *got.OperatorID != f.ownerID {
		t.Errorf("manual checkin must record operator: %+v", got.OperatorID)
	}
}

// TestUC032_PIN_MatchesAndRejects walks: assign PIN → wrong PIN fails → right
// PIN succeeds → 5 wrong attempts trigger lockout.
func TestUC032_PIN_MatchesAndRejects(t *testing.T) {
	f := setupCheckinsFixture(t)
	ctx := context.Background()

	// Assign a PIN.
	assignPin := memApp.NewAssignPin(f.memberRepo, f.uow, f.recorder)
	pinOut, err := assignPin.Execute(ctx, memApp.AssignPinInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		PlainPin: "4729",
	})
	if err != nil {
		t.Fatalf("assignPin: %v", err)
	}
	if pinOut.Pin != "4729" {
		t.Errorf("PIN echo mismatch: %s", pinOut.Pin)
	}

	limiter := chkApp.NewPinAttemptLimiter()
	uc := chkApp.NewCheckinByPin(f.memberSvc, f.memberRepo, f.checkinRepo, f.uow, f.recorder, limiter)

	// Wrong PIN → BusinessError.
	if _, err := uc.Execute(ctx, chkApp.CheckinByPinInput{GymID: f.gymID, Pin: "0000"}); err == nil {
		t.Errorf("wrong PIN should fail")
	}

	// Right PIN → success.
	view, err := uc.Execute(ctx, chkApp.CheckinByPinInput{GymID: f.gymID, Pin: "4729"})
	if err != nil {
		t.Fatalf("right PIN: %v", err)
	}
	if view.Method != "pin" || view.Result != "allowed_active" {
		t.Errorf("PIN checkin payload wrong: %+v", view)
	}

	// 5 wrong attempts trigger lockout.
	for i := 0; i < 5; i++ {
		_, _ = uc.Execute(ctx, chkApp.CheckinByPinInput{GymID: f.gymID, Pin: "1111"})
	}
	if !limiter.IsBlocked(f.gymID, time.Now()) {
		t.Errorf("after 5 failures the gym should be blocked")
	}
}

// TestDA29_2_OverrideAddsRowAndKeepsOriginal: deny path + override appends a
// second row with allowed_override; original (denied) row must remain.
func TestDA29_2_OverrideAfterDenied(t *testing.T) {
	f := setupCheckinsFixture(t)
	ctx := context.Background()

	// Make the member inactive so manual checkin returns denied_inactive.
	toggleMember := memApp.NewToggleMemberStatus(f.memberRepo, f.uow, f.recorder)
	if _, err := toggleMember.Execute(ctx, memApp.ToggleMemberStatusInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewStatus: "inactive", Reason: "test",
	}); err != nil {
		t.Fatalf("toggle: %v", err)
	}

	manual := chkApp.NewCheckinManual(f.memberSvc, f.checkinRepo, f.uow, f.recorder)
	deniedView, err := manual.Execute(ctx, chkApp.CheckinManualInput{
		GymID: f.gymID, OperatorID: f.ownerID, MemberID: f.memberID,
	})
	if err != nil {
		t.Fatalf("manual denied: %v", err)
	}
	if deniedView.Result != "denied_inactive" {
		t.Errorf("expected denied_inactive, got %s", deniedView.Result)
	}

	override := chkApp.NewOverrideCheckin(f.memberSvc, f.checkinRepo, f.uow, f.recorder)
	overrideView, err := override.Execute(ctx, chkApp.OverrideCheckinInput{
		GymID: f.gymID, OperatorID: f.ownerID, MemberID: f.memberID,
		OriginalMethod: "manual", Reason: "el dueño autorizó verbalmente",
	})
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if overrideView.Result != "allowed_override" {
		t.Errorf("expected allowed_override, got %s", overrideView.Result)
	}
	if !overrideView.Override {
		t.Errorf("Override flag should be set")
	}

	// Both rows should exist.
	tx, _ := f.uow.Query(ctx)
	denied, _ := f.checkinRepo.GetByID(tx, deniedView.CheckinID)
	overridden, _ := f.checkinRepo.GetByID(tx, overrideView.CheckinID)
	if denied == nil || denied.Result != "denied_inactive" {
		t.Errorf("original denied row missing or mutated")
	}
	if overridden == nil || !overridden.ManualOverride || overridden.OverrideReason == nil {
		t.Errorf("override row not persisted correctly: %+v", overridden)
	}
}

// TestKioskLoop_ConnectDisconnectEvents verifies the broadcaster fires the
// expected events when the mock reader simulates hotplug.
func TestKioskLoop_BroadcastsHotplugEvents(t *testing.T) {
	f := setupCheckinsFixture(t)

	checkin := chkApp.NewCheckinByFingerprint(f.memberSvc, f.checkinRepo, f.reader, f.uow, f.recorder)
	events := chkApp.NewKioskBroadcaster()
	loop := chkApp.NewKioskLoop(f.gymID, f.reader, checkin, events)
	loop.CaptureBackoff = 5 * time.Millisecond

	sub, cancel := events.Subscribe()
	defer cancel()

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()

	if err := loop.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer loop.Stop()

	// Wait for the kiosk_started event.
	select {
	case evt := <-sub:
		if evt.Type != chkApp.EventKioskStarted {
			t.Errorf("first event should be kiosk_started, got %s", evt.Type)
		}
	case <-time.After(time.Second):
		t.Fatalf("timeout waiting for kiosk_started")
	}

	// Simulate disconnect — should fan out reader_disconnected.
	f.reader.SimulateDisconnect()
	if !waitForEvent(sub, chkApp.EventReaderDisconnected, time.Second) {
		t.Errorf("reader_disconnected not received")
	}
	f.reader.SimulateConnect()
	if !waitForEvent(sub, chkApp.EventReaderConnected, time.Second) {
		t.Errorf("reader_connected not received")
	}
}

func waitForEvent(sub <-chan chkApp.KioskEvent, want chkApp.KioskEventType, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-sub:
			if evt.Type == want {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
