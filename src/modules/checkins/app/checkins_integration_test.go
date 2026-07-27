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
	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
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
// up + one membership type + one member with an active membership, plus the
// tinta-bio stack in mock form: MockEngine + BiometricHub + use cases. En
// mock-land un dedo ES su string de FMD (ver biometric.MockEngine).
type checkinsFixture struct {
	t        *testing.T
	uow      sharedDomain.UnitOfWork
	recorder audit.Recorder
	gymID    uuid.UUID
	ownerID  uuid.UUID
	memberID uuid.UUID

	memberRepo      *memRepoLite.MemberSQLiteRepository
	membershipRepo  *memRepoLite.MembershipSQLiteRepository
	mtID            uuid.UUID
	fingerprintRepo *memRepoLite.FingerprintSQLiteRepository
	checkinRepo     *chkRepoLite.CheckinSQLiteRepository
	memberSvc       *memApp.MemberService

	gmkProvider *bcrypto.InMemoryGMKProvider
	engine      *biometric.MockEngine
	events      *chkApp.BioBroadcaster
	hub         *chkApp.BiometricHub
	registerFP  *memApp.RegisterFingerprint
	checkinFP   *chkApp.CheckinByFingerprint
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

	// Todas las migraciones en orden — no un subset cherry-picked. Así el
	// schema del test matchea producción y no se rompe cada vez que una
	// migración agrega una columna a una tabla base (p.ej. member_number de
	// ADR-010). os.ReadDir ordena por nombre y los archivos están zero-padded
	// (001_..) → orden correcto.
	migDir := "../../../../db_migrations/sqlite"
	migEntries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range migEntries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		m := filepath.Join(migDir, e.Name())
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

	gmkProvider := bcrypto.NewInMemoryGMKProvider()
	gmkProvider.SetDeterministic(out.GymID, "test-gmk-seed")

	engine := biometric.NewMockEngine()
	events := chkApp.NewBioBroadcaster()
	registerFP := memApp.NewRegisterFingerprint(memberRepo, fpRepo, gmkProvider, uow, recorder)
	checkinFP := chkApp.NewCheckinByFingerprint(memberSvc, checkinRepo, uow, recorder)
	hub := chkApp.NewBiometricHub(engine, checkinFP, registerFP, memberRepo, fpRepo, gmkProvider, uow, events)
	registerFP.WithMatcher(hub)

	f := &checkinsFixture{
		t:               t,
		uow:             uow,
		recorder:        recorder,
		gymID:           out.GymID,
		ownerID:         out.UserID,
		memberRepo:      memberRepo,
		membershipRepo:  membershipRepo,
		mtID:            mtOut.ID,
		fingerprintRepo: fpRepo,
		checkinRepo:     checkinRepo,
		memberSvc:       memberSvc,
		gmkProvider:     gmkProvider,
		engine:          engine,
		events:          events,
		hub:             hub,
		registerFP:      registerFP,
		checkinFP:       checkinFP,
	}
	f.memberID = f.addActiveMember("Juan Pérez", "5551234567")

	// El hub sigue la sesión del operador: gym activo → galería al helper
	// (vacía todavía — nadie enrolado).
	hub.SetActiveGym(out.GymID)
	return f
}

// addActiveMember creates a member and activates their membership (el alta
// nace pending_payment; lo activamos vía el seam RenewMembershipForPayment).
func (f *checkinsFixture) addActiveMember(name, phone string) uuid.UUID {
	f.t.Helper()
	createMember := memApp.NewCreateMember(f.memberRepo, f.membershipRepo,
		memRepoLite.NewMembershipTypeSQLiteRepository(), f.uow, f.recorder)
	mOut, err := createMember.Execute(context.Background(), memApp.CreateMemberInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		FullName: name, Phone: phone,
		MembershipTypeID: f.mtID, StartDate: time.Now().UTC(),
	})
	if err != nil {
		f.t.Fatalf("createMember %s: %v", name, err)
	}
	if err := f.uow.Command(context.Background(), func(tx sharedDomain.Transaction) error {
		_, err := f.memberSvc.RenewMembershipForPayment(context.Background(), tx, memApp.RenewMembershipForPaymentInput{
			MemberID: mOut.MemberID, MembershipTypeID: f.mtID, PaymentDate: time.Now().UTC(),
		}, time.Now().UTC())
		return err
	}); err != nil {
		f.t.Fatalf("activate membership %s: %v", name, err)
	}
	return mOut.MemberID
}

// enrollViaSession walks the full session flow for a member: start → N
// dedazos → enroll_completed. Returns the completed event.
func (f *checkinsFixture) enrollViaSession(memberID uuid.UUID, fmd string) chkApp.BioEvent {
	f.t.Helper()
	sub, cancel := f.events.Subscribe()
	defer cancel()

	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: memberID, ConsentAccepted: true,
	}); err != nil {
		f.t.Fatalf("StartEnroll: %v", err)
	}
	for i := 0; i < f.hub.RequiredSamples; i++ {
		f.hub.HandleSample(fmd, "DP_QUALITY_GOOD")
	}
	evt, ok := waitForEvent(sub, chkApp.BioEnrollCompleted, time.Second)
	if !ok {
		f.t.Fatalf("enroll_completed no llegó")
	}
	return evt
}

// waitForEvent drains sub until it sees `want` or the timeout fires.
func waitForEvent(sub <-chan chkApp.BioEvent, want chkApp.BioEventType, timeout time.Duration) (chkApp.BioEvent, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-sub:
			if evt.Type == want {
				return evt, true
			}
		case <-deadline:
			return chkApp.BioEvent{}, false
		}
	}
}

// ───────────────────────── enroll + checkin (UC-028 / UC-029) ─────────────────

// TestUC028AndUC029_EnrollSessionAndCheckin walks the inverted flow end to
// end: sesión de enroll (3 dedazos → FMD de enrollment → cifrado + guardado
// + galería nueva) y luego un dedazo del mismo dedo → identify → checkin.
func TestUC028AndUC029_EnrollSessionAndCheckin(t *testing.T) {
	f := setupCheckinsFixture(t)
	ctx := context.Background()
	fingerJuan := "fmd-opaco-juan-0001"

	sub, cancelSub := f.events.Subscribe()
	defer cancelSub()

	completed := f.enrollViaSession(f.memberID, fingerJuan)
	if completed.Enroll == nil || completed.Enroll.MemberID != f.memberID {
		t.Fatalf("enroll_completed payload raro: %+v", completed.Enroll)
	}
	if len(completed.Enroll.FingerprintIDs) != 1 {
		t.Errorf("esperaba 1 fingerprint id, got %d", len(completed.Enroll.FingerprintIDs))
	}

	// El template quedó cifrado con la GMK y el contenido es el FMD opaco.
	tx, _ := f.uow.Query(ctx)
	rows, err := f.fingerprintRepo.ListByMember(tx, f.memberID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("fingerprints persistidas: %d err=%v", len(rows), err)
	}
	if rows[0].TemplateFormat != biometric.FormatFMD {
		t.Errorf("template_format = %q, want %q", rows[0].TemplateFormat, biometric.FormatFMD)
	}
	gmk, _ := f.gmkProvider.GetGMK(ctx, f.gymID)
	plain, err := bcrypto.DecryptTemplate(gmk, rows[0].TemplateEncrypted)
	if err != nil || string(plain) != fingerJuan {
		t.Errorf("template descifrado = %q err=%v, want %q", plain, err, fingerJuan)
	}

	// La galería del helper ya trae al socio (ref = fingerprint id).
	if len(f.engine.Gallery) != 1 || f.engine.Gallery[0].FMD != fingerJuan {
		t.Fatalf("galería del mock no refrescada: %+v", f.engine.Gallery)
	}

	// UC-029: dedazo del mismo dedo → identify → checkin registrado.
	f.hub.HandleSample(fingerJuan, "DP_QUALITY_GOOD")
	evt, ok := waitForEvent(sub, chkApp.BioCheckinResult, time.Second)
	if !ok {
		t.Fatalf("checkin_result no llegó")
	}
	view := evt.Checkin
	if view == nil || view.MemberID != f.memberID {
		t.Fatalf("checkin de socio equivocado: %+v", view)
	}
	if view.Method != "fingerprint" {
		t.Errorf("method = %s, want fingerprint", view.Method)
	}
	if view.AccessStatus != access.AllowedActive || view.Result != "allowed_active" {
		t.Errorf("acceso = %s/%s, want allowed_active", view.AccessStatus, view.Result)
	}

	// Persistido de verdad.
	tx2, _ := f.uow.Query(ctx)
	got, err := f.checkinRepo.GetByID(tx2, view.CheckinID)
	if err != nil || got.MemberID != f.memberID || got.Method != "fingerprint" {
		t.Errorf("checkin persistido mal: %+v err=%v", got, err)
	}
}

// TestUC029_NoMatch_PublishesEventWithoutRow: dedo desconocido → evento
// checkin_no_match y CERO filas (UC-029 sin coincidencia no registra).
func TestUC029_NoMatch_PublishesEventWithoutRow(t *testing.T) {
	f := setupCheckinsFixture(t)
	f.enrollViaSession(f.memberID, "fmd-juan")

	sub, cancel := f.events.Subscribe()
	defer cancel()
	f.hub.HandleSample("fmd-de-un-desconocido", "DP_QUALITY_GOOD")
	if _, ok := waitForEvent(sub, chkApp.BioCheckinNoMatch, time.Second); !ok {
		t.Fatalf("checkin_no_match no llegó")
	}

	tx, _ := f.uow.Query(context.Background())
	n, err := f.checkinRepo.CountTodayByGym(tx, f.gymID, time.Now().UTC())
	if err != nil || n != 0 {
		t.Errorf("no-match no debe registrar checkin, count=%d err=%v", n, err)
	}
}

// TestUC029_EmptyGallery_NoMatchSinMotor: gym sin NADIE enrolado (galería
// vacía ya enviada al helper) → un dedazo publica checkin_no_match con el
// mensaje "nadie tiene huella registrada" SIN llamar Identify: el SDK real
// con 0 candidatos devuelve DP_INVALID_PARAMETER, no "sin matches", y eso
// salía al FE como checkin_error de lector (mordió en la validación real).
func TestUC029_EmptyGallery_NoMatchSinMotor(t *testing.T) {
	f := setupCheckinsFixture(t)

	sub, cancel := f.events.Subscribe()
	defer cancel()
	identifyBefore := f.engine.IdentifyCalls
	f.hub.HandleSample("fmd-de-un-desconocido", "DP_QUALITY_GOOD")
	evt, ok := waitForEvent(sub, chkApp.BioCheckinNoMatch, time.Second)
	if !ok {
		t.Fatalf("checkin_no_match no llegó con galería vacía")
	}
	if evt.Message != chkErrors.ErrFingerprintNotEnrolled.Error() {
		t.Errorf("mensaje equivocado: %q", evt.Message)
	}
	if f.engine.IdentifyCalls != identifyBefore {
		t.Errorf("con galería vacía NO se debe llamar identify (el SDK real truena con 0 candidatos)")
	}
}

// TestHub_SamplesDuringEnrollDoNotIdentify: con sesión abierta, un dedazo de
// un socio YA enrolado se acumula para el enroll en lugar de hacer checkin.
func TestHub_SamplesDuringEnrollDoNotIdentify(t *testing.T) {
	f := setupCheckinsFixture(t)
	f.enrollViaSession(f.memberID, "fmd-juan")
	otherID := f.addActiveMember("Pedro Soto", "5559998888")

	sub, cancel := f.events.Subscribe()
	defer cancel()
	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: otherID, ConsentAccepted: true,
	}); err != nil {
		t.Fatalf("StartEnroll: %v", err)
	}
	// Dedazo del dedo de Juan (enrolado) — debe ir a la sesión de Pedro, no
	// al kiosko. (Al final colisionará, pero eso es exactamente el punto.)
	f.hub.HandleSample("fmd-juan", "DP_QUALITY_GOOD")
	if evt, ok := waitForEvent(sub, chkApp.BioEnrollProgress, time.Second); !ok || evt.Enroll.Captured != 1 {
		t.Fatalf("el sample no se acumuló en la sesión: %+v", evt)
	}

	tx, _ := f.uow.Query(context.Background())
	n, _ := f.checkinRepo.CountTodayByGym(tx, f.gymID, time.Now().UTC())
	if n != 0 {
		t.Errorf("un sample durante enroll jamás debe registrar checkin, count=%d", n)
	}
}

// TestHub_EnrollCollision: el FMD de enrollment matchea a OTRO socio → evento
// enroll_failed con el contrato fingerprint_collision (id + nombre) y nada
// persistido para el segundo socio.
func TestHub_EnrollCollision(t *testing.T) {
	f := setupCheckinsFixture(t)
	f.enrollViaSession(f.memberID, "fmd-juan")
	otherID := f.addActiveMember("Pedro Soto", "5559998888")

	sub, cancel := f.events.Subscribe()
	defer cancel()
	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: otherID, ConsentAccepted: true,
	}); err != nil {
		t.Fatalf("StartEnroll: %v", err)
	}
	for i := 0; i < f.hub.RequiredSamples; i++ {
		f.hub.HandleSample("fmd-juan", "DP_QUALITY_GOOD")
	}
	evt, ok := waitForEvent(sub, chkApp.BioEnrollFailed, time.Second)
	if !ok {
		t.Fatalf("enroll_failed no llegó")
	}
	if evt.Code != chkApp.EnrollFailCollision {
		t.Fatalf("code = %q, want fingerprint_collision", evt.Code)
	}
	if evt.Enroll == nil || evt.Enroll.Data["existing_member_id"] != f.memberID.String() {
		t.Errorf("payload de colisión sin existing_member_id: %+v", evt.Enroll)
	}
	if evt.Enroll.Data["existing_member_name"] != "Juan Pérez" {
		t.Errorf("payload de colisión sin existing_member_name: %+v", evt.Enroll.Data)
	}

	tx, _ := f.uow.Query(context.Background())
	rows, _ := f.fingerprintRepo.ListByMember(tx, otherID)
	if len(rows) != 0 {
		t.Errorf("la colisión no debe persistir huella, got %d", len(rows))
	}
	if f.hub.Enrolling() {
		t.Errorf("la sesión debe cerrarse tras la colisión")
	}
}

// TestHub_EnrollTimeout: sesión sin dedazos suficientes expira sola y el
// siguiente sample vuelve al modo check-in.
func TestHub_EnrollTimeout(t *testing.T) {
	f := setupCheckinsFixture(t)
	f.enrollViaSession(f.memberID, "fmd-juan")
	otherID := f.addActiveMember("Pedro Soto", "5559998888")

	f.hub.EnrollTTL = 50 * time.Millisecond
	sub, cancel := f.events.Subscribe()
	defer cancel()
	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: otherID, ConsentAccepted: true,
	}); err != nil {
		t.Fatalf("StartEnroll: %v", err)
	}
	evt, ok := waitForEvent(sub, chkApp.BioEnrollFailed, time.Second)
	if !ok || evt.Code != chkApp.EnrollFailTimeout {
		t.Fatalf("esperaba enroll_failed timeout, got %+v ok=%v", evt, ok)
	}

	// De vuelta en modo check-in: el dedo de Juan identifica normal.
	f.hub.HandleSample("fmd-juan", "DP_QUALITY_GOOD")
	if _, ok := waitForEvent(sub, chkApp.BioCheckinResult, time.Second); !ok {
		t.Fatalf("tras timeout el kiosko debe volver a identificar")
	}
}

// TestHub_EnrollCancel: cancelación explícita cierra la sesión y publica el
// evento; cancelar sin sesión regresa error de negocio.
func TestHub_EnrollCancel(t *testing.T) {
	f := setupCheckinsFixture(t)
	sub, cancel := f.events.Subscribe()
	defer cancel()

	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID, ConsentAccepted: true,
	}); err != nil {
		t.Fatalf("StartEnroll: %v", err)
	}
	if err := f.hub.CancelEnroll(context.Background()); err != nil {
		t.Fatalf("CancelEnroll: %v", err)
	}
	evt, ok := waitForEvent(sub, chkApp.BioEnrollFailed, time.Second)
	if !ok || evt.Code != chkApp.EnrollFailCancelled {
		t.Fatalf("esperaba enroll_failed cancelled, got %+v", evt)
	}
	if err := f.hub.CancelEnroll(context.Background()); err == nil {
		t.Errorf("cancelar sin sesión debe fallar")
	}
}

// TestHub_EnrollRequiresConsentAndSingleSession: validaciones fail-fast del
// start (consentimiento, sesión duplicada, huella previa).
func TestHub_EnrollStartValidations(t *testing.T) {
	f := setupCheckinsFixture(t)

	// Sin consentimiento.
	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
	}); err == nil {
		t.Errorf("start sin consentimiento debe fallar")
	}

	// Sesión duplicada.
	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID, ConsentAccepted: true,
	}); err != nil {
		t.Fatalf("primer start: %v", err)
	}
	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID, ConsentAccepted: true,
	}); err == nil {
		t.Errorf("segunda sesión simultánea debe fallar")
	}
	_ = f.hub.CancelEnroll(context.Background())

	// Huella previa → delete-first (mismo contrato que RegisterFingerprint).
	f.enrollViaSession(f.memberID, "fmd-juan")
	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID, ConsentAccepted: true,
	}); err == nil {
		t.Errorf("start con huella existente debe fallar (delete-first)")
	}
}

// TestHub_GalleryEpochRace: el helper contesta identify con un epoch viejo
// (carrera enroll-vs-identify / helper recién reiniciado) → el hub re-manda
// la galería y reintenta — el checkin sale bien en el segundo intento.
func TestHub_GalleryEpochRace(t *testing.T) {
	f := setupCheckinsFixture(t)
	f.enrollViaSession(f.memberID, "fmd-juan")

	// Desincronizar: el mock conserva la galería pero con epoch VIEJO — la
	// carrera enroll-vs-identify real (el hub re-mandó galería nueva y el
	// helper contesta todavía con la anterior). No se vacía la galería: el
	// mock fiel al SDK truena con 0 candidatos, y el caso "helper
	// reiniciado con galería vacía" ya lo cubren el short-circuit de
	// identify() + el resend de HandleHelperUp.
	f.engine.GalleryEpoch = "epoch-viejo"
	callsBefore := f.engine.SetGalleryCalls

	sub, cancel := f.events.Subscribe()
	defer cancel()
	f.hub.HandleSample("fmd-juan", "DP_QUALITY_GOOD")
	evt, ok := waitForEvent(sub, chkApp.BioCheckinResult, time.Second)
	if !ok {
		t.Fatalf("checkin_result no llegó tras la carrera de epoch")
	}
	if evt.Checkin.MemberID != f.memberID {
		t.Errorf("socio equivocado tras retry: %+v", evt.Checkin)
	}
	if f.engine.SetGalleryCalls <= callsBefore {
		t.Errorf("el hub debió re-mandar la galería (calls %d → %d)", callsBefore, f.engine.SetGalleryCalls)
	}
}

// TestHub_HelperRestartMidEnroll: el helper muere y revive a media sesión —
// la galería se re-manda al volver y los FMDs ya acumulados siguen valiendo.
func TestHub_HelperRestartMidEnroll(t *testing.T) {
	f := setupCheckinsFixture(t)
	otherFinger := "fmd-pedro"
	otherID := f.addActiveMember("Pedro Soto", "5559998888")

	sub, cancel := f.events.Subscribe()
	defer cancel()
	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: otherID, ConsentAccepted: true,
	}); err != nil {
		t.Fatalf("StartEnroll: %v", err)
	}
	f.hub.HandleSample(otherFinger, "DP_QUALITY_GOOD")

	// Crash + respawn del helper (los eventos llegan serializados del engine).
	callsBefore := f.engine.SetGalleryCalls
	f.hub.HandleHelperDown("proceso murió")
	f.hub.HandleHelperUp()
	if f.engine.SetGalleryCalls <= callsBefore {
		t.Errorf("HandleHelperUp debe re-mandar la galería")
	}

	// La sesión sigue viva: los dedazos restantes completan el enroll.
	for i := 1; i < f.hub.RequiredSamples; i++ {
		f.hub.HandleSample(otherFinger, "DP_QUALITY_GOOD")
	}
	if _, ok := waitForEvent(sub, chkApp.BioEnrollCompleted, time.Second); !ok {
		t.Fatalf("la sesión debió sobrevivir el restart del helper")
	}
}

// TestHub_EnrollInvalidSet: el helper rechaza el set (DP_ENROLLMENT_INVALID_
// SET) → enroll_failed enrollment_invalid y la sesión se CIERRA (todo
// enroll_failed es terminal: el FE suelta su session_id al recibirlo; una
// sesión que sobreviviera se tragaría los dedazos de check-in hasta el TTL).
// El reintento abre sesión NUEVA y completa.
func TestHub_EnrollInvalidSet(t *testing.T) {
	f := setupCheckinsFixture(t)

	sub, cancel := f.events.Subscribe()
	defer cancel()
	if _, err := f.hub.StartEnroll(context.Background(), chkApp.StartEnrollInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID, ConsentAccepted: true,
	}); err != nil {
		t.Fatalf("StartEnroll: %v", err)
	}
	f.engine.EnrollErr = &biometric.CommandError{Code: "DP_ENROLLMENT_INVALID_SET"}
	for i := 0; i < f.hub.RequiredSamples; i++ {
		f.hub.HandleSample("fmd-borroso", "DP_QUALITY_GOOD")
	}
	evt, ok := waitForEvent(sub, chkApp.BioEnrollFailed, time.Second)
	if !ok || evt.Code != chkApp.EnrollFailInvalidSet {
		t.Fatalf("esperaba enrollment_invalid, got %+v", evt)
	}
	if f.hub.Enrolling() {
		t.Fatalf("la sesión debe CERRARSE tras set inválido (enroll_failed es terminal)")
	}

	// El siguiente dedazo ya NO es de enroll: vuelve al modo check-in.
	f.hub.HandleSample("fmd-borroso", "DP_QUALITY_GOOD")
	if _, ok := waitForEvent(sub, chkApp.BioCheckinAttempt, time.Second); !ok {
		t.Fatalf("tras el fallo los dedazos deben fluir al check-in, no a la sesión muerta")
	}

	// Reintento del operador = sesión nueva, con el helper ya cooperando.
	f.engine.EnrollErr = nil
	f.enrollViaSession(f.memberID, "fmd-juan")
}

// TestHub_ReaderStateEvents: hot-plug del lector fluye a los subscribers
// (reemplaza al viejo test del KioskLoop).
func TestHub_ReaderStateEvents(t *testing.T) {
	f := setupCheckinsFixture(t)
	sub, cancel := f.events.Subscribe()
	defer cancel()

	f.hub.HandleReaderState(false, "", "")
	if _, ok := waitForEvent(sub, chkApp.BioReaderDisconnected, time.Second); !ok {
		t.Errorf("reader_disconnected no llegó")
	}
	f.hub.HandleReaderState(true, "U.are.U 4500", "S123")
	evt, ok := waitForEvent(sub, chkApp.BioReaderConnected, time.Second)
	if !ok || evt.ReaderName != "U.are.U 4500" {
		t.Errorf("reader_connected sin nombre: %+v", evt)
	}
}

// TestHub_LogoutClearsGallery: gym Nil (logout) manda galería vacía y los
// dedazos dejan de identificar sin tirar errores.
func TestHub_LogoutClearsGallery(t *testing.T) {
	f := setupCheckinsFixture(t)
	f.enrollViaSession(f.memberID, "fmd-juan")

	f.hub.SetActiveGym(uuid.Nil)
	if len(f.engine.Gallery) != 0 {
		t.Fatalf("logout debe limpiar la galería del helper, got %d", len(f.engine.Gallery))
	}

	sub, cancel := f.events.Subscribe()
	defer cancel()
	f.hub.HandleSample("fmd-juan", "DP_QUALITY_GOOD")
	select {
	case evt := <-sub:
		t.Errorf("sin gym activo no debe haber eventos de checkin, got %+v", evt)
	case <-time.After(100 * time.Millisecond):
	}
}

// ───────────────────────── checkin manual / número / override ─────────────────

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

// TestUC032_Number_MatchesAndRejects walks (ADR-010): assign número → número
// incorrecto falla → número correcto entra → suficientes intentos incorrectos
// disparan el lockout anti-enumeración.
func TestUC032_Number_MatchesAndRejects(t *testing.T) {
	f := setupCheckinsFixture(t)
	ctx := context.Background()

	// Asignar número de socio explícito.
	assignNumber := memApp.NewAssignMemberNumber(f.memberRepo, f.uow, f.recorder)
	out, err := assignNumber.Execute(ctx, memApp.AssignMemberNumberInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		Number: 4729,
	})
	if err != nil {
		t.Fatalf("assignNumber: %v", err)
	}
	if out.MemberNumber != 4729 {
		t.Errorf("number echo mismatch: %d", out.MemberNumber)
	}

	limiter := chkApp.NewNumberAttemptLimiter()
	uc := chkApp.NewCheckinByNumber(f.memberSvc, f.memberRepo, f.checkinRepo, f.uow, f.recorder, limiter)

	// Número incorrecto → BusinessError.
	if _, err := uc.Execute(ctx, chkApp.CheckinByNumberInput{GymID: f.gymID, Number: 1111}); err == nil {
		t.Errorf("wrong number should fail")
	}

	// Número correcto → success.
	view, err := uc.Execute(ctx, chkApp.CheckinByNumberInput{GymID: f.gymID, Number: 4729})
	if err != nil {
		t.Fatalf("right number: %v", err)
	}
	if view.Method != "number" || view.Result != "allowed_active" {
		t.Errorf("number checkin payload wrong: %+v", view)
	}

	// Suficientes intentos incorrectos disparan el lockout (max=10 en el
	// limiter anti-enumeración relajado de ADR-010).
	for i := 0; i < 10; i++ {
		_, _ = uc.Execute(ctx, chkApp.CheckinByNumberInput{GymID: f.gymID, Number: 1111})
	}
	if !limiter.IsBlocked(f.gymID, time.Now()) {
		t.Errorf("tras 10 fallos el gym debería estar bloqueado")
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

// El checkin por huella con socio inactivo sí identifica (la galería filtra
// por status, pero puede estar desactualizada) — la decisión de acceso al
// momento del checkin es la autoridad y devuelve denied.
func TestHub_StaleGalleryStillDeniesInactiveMember(t *testing.T) {
	f := setupCheckinsFixture(t)
	f.enrollViaSession(f.memberID, "fmd-juan")

	// Toggle a inactivo SIN refrescar galería (simula la ventana entre el
	// toggle y el refresh periódico — el mock conserva el candidato).
	toggleMember := memApp.NewToggleMemberStatus(f.memberRepo, f.uow, f.recorder)
	if _, err := toggleMember.Execute(context.Background(), memApp.ToggleMemberStatusInput{
		GymID: f.gymID, ActorUserID: f.ownerID, MemberID: f.memberID,
		NewStatus: "inactive", Reason: "baja temporal",
	}); err != nil {
		t.Fatalf("toggle: %v", err)
	}

	sub, cancel := f.events.Subscribe()
	defer cancel()
	f.hub.HandleSample("fmd-juan", "DP_QUALITY_GOOD")
	evt, ok := waitForEvent(sub, chkApp.BioCheckinResult, time.Second)
	if !ok {
		t.Fatalf("checkin_result no llegó")
	}
	if evt.Checkin.Result != "denied_inactive" {
		t.Errorf("galería vieja no debe regalar acceso: result=%s", evt.Checkin.Result)
	}
}
