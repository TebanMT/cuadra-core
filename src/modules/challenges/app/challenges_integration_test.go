//go:build sidecar

package app_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	challengesApp "github.com/cuadra/cuadra-core/src/modules/challenges/app"
	categoryDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/category"
	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	challengeInfra "github.com/cuadra/cuadra-core/src/modules/challenges/infraestructure"
	challengeRepoLite "github.com/cuadra/cuadra-core/src/modules/challenges/infraestructure/db/repositories"
	gymRepoLite "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memRepoLite "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/repositories"
	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	usersRepoLite "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// challengesFixture spins up a fresh sqlite + owner + 6 members + challenge
// in open_registration + 2 categories with 3 members each. Every test
// starts from this clean slate.
type challengesFixture struct {
	t          *testing.T
	db         *sqlx.DB
	uow        sharedDomain.UnitOfWork
	recorder   audit.Recorder
	gymID      uuid.UUID
	ownerID    uuid.UUID
	challenge  *challengeDomain.Challenge
	catA       *categoryDomain.Category // 3 members
	catB       *categoryDomain.Category // 3 members
	partsA     []*participantDomain.Participant
	partsB     []*participantDomain.Participant
	chRepo     *challengeRepoLite.ChallengeSQLiteRepository
	catRepo    *challengeRepoLite.CategorySQLiteRepository
	partRepo   *challengeRepoLite.ParticipantSQLiteRepository
	mRepo      *challengeRepoLite.MeasurementSQLiteRepository
	attendance *challengeInfra.CheckinsAttendanceAdapter
}

func setupChallenges(t *testing.T) *challengesFixture {
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
		m := filepath.Join(migDir, e.Name())
		schema, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		if _, err := db.Exec(string(schema)); err != nil {
			t.Fatalf("apply %s: %v", m, err)
		}
	}
	uow := sharedDomain.NewSQLiteUnitOfWork(db, nil) // no sync queue — focus on local state
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

	mtRepo := memRepoLite.NewMembershipTypeSQLiteRepository()
	memberRepo := memRepoLite.NewMemberSQLiteRepository()
	membershipRepo := memRepoLite.NewMembershipSQLiteRepository()

	createMT := memApp.NewCreateMembershipType(mtRepo, uow, recorder)
	mt, err := createMT.Execute(context.Background(), memApp.CreateMembershipTypeInput{
		GymID: owner.GymID, ActorUserID: owner.UserID,
		Name: "Mensual", Price: 500, DurationDays: 30,
		EnrollmentFee: 100,
	})
	if err != nil {
		t.Fatalf("create mt: %v", err)
	}

	// Build six members. Numbers in the names so failures are easy to read.
	createMember := memApp.NewCreateMember(memberRepo, membershipRepo, mtRepo, uow, recorder)
	memberIDs := make([]uuid.UUID, 0, 6)
	for i := 0; i < 6; i++ {
		mem, err := createMember.Execute(context.Background(), memApp.CreateMemberInput{
			GymID: owner.GymID, ActorUserID: owner.UserID,
			FullName:         fmt.Sprintf("Socio %d", i+1),
			Phone:            fmt.Sprintf("+5244400000%02d", i+1),
			MembershipTypeID: mt.ID,
			StartDate:        time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create member %d: %v", i, err)
		}
		memberIDs = append(memberIDs, mem.MemberID)
	}

	// Challenge repositories.
	chRepo := challengeRepoLite.NewChallengeSQLiteRepository()
	catRepo := challengeRepoLite.NewCategorySQLiteRepository()
	partRepo := challengeRepoLite.NewParticipantSQLiteRepository()
	mRepo := challengeRepoLite.NewMeasurementSQLiteRepository()
	attendance := challengeInfra.NewCheckinsAttendanceAdapter()

	// Create a draft challenge with dates pinned around a known moment so the
	// T₀/T₁ windows are reachable from the fixed `now` used in tests.
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	startsAt := now.Add(-7 * 24 * time.Hour) // started a week ago
	t0Deadline := now.Add(14 * 24 * time.Hour)
	t1Start := now.Add(20 * 24 * time.Hour)
	endsAt := now.Add(30 * 24 * time.Hour)
	createChallenge := challengesApp.NewCreateChallenge(chRepo, uow, recorder)
	createChallenge.NowFunc = func() time.Time { return now }
	ch, err := createChallenge.Execute(context.Background(), challengesApp.CreateChallengeInput{
		GymID:                 owner.GymID,
		ActorUserID:           owner.UserID,
		Name:                  "Reto 12 — Edición 1",
		StartsAt:              startsAt,
		MeasurementT0Deadline: t0Deadline,
		MeasurementT1Start:    t1Start,
		EndsAt:                endsAt,
	})
	if err != nil {
		t.Fatalf("create challenge: %v", err)
	}

	// Two categories.
	addCat := challengesApp.NewAddCategory(chRepo, catRepo, uow, recorder)
	addCat.NowFunc = func() time.Time { return now }
	catA, err := addCat.Execute(context.Background(), challengesApp.AddCategoryInput{
		GymID: owner.GymID, ActorUserID: owner.UserID,
		ChallengeID: ch.ID, Name: "Hombres", SortOrder: 1,
	})
	if err != nil {
		t.Fatalf("add catA: %v", err)
	}
	catB, err := addCat.Execute(context.Background(), challengesApp.AddCategoryInput{
		GymID: owner.GymID, ActorUserID: owner.UserID,
		ChallengeID: ch.ID, Name: "Mujeres", SortOrder: 2,
	})
	if err != nil {
		t.Fatalf("add catB: %v", err)
	}

	// Open registration so AddParticipant succeeds.
	transition := challengesApp.NewTransitionChallengeStatus(chRepo, catRepo, uow, recorder)
	transition.NowFunc = func() time.Time { return now }
	if _, err := transition.Execute(context.Background(), challengesApp.TransitionChallengeStatusInput{
		GymID: owner.GymID, ActorUserID: owner.UserID,
		ChallengeID: ch.ID, Transition: challengesApp.TransitionOpenRegistration,
	}); err != nil {
		t.Fatalf("open registration: %v", err)
	}

	addPart := challengesApp.NewAddParticipant(chRepo, catRepo, partRepo, uow, recorder)
	addPart.NowFunc = func() time.Time { return now }
	partsA := make([]*participantDomain.Participant, 0, 3)
	partsB := make([]*participantDomain.Participant, 0, 3)
	for i, mid := range memberIDs {
		var cat *categoryDomain.Category
		if i < 3 {
			cat = catA
		} else {
			cat = catB
		}
		p, err := addPart.Execute(context.Background(), challengesApp.AddParticipantInput{
			GymID: owner.GymID, ActorUserID: owner.UserID,
			ChallengeID: ch.ID, MemberID: mid, CategoryID: cat.ID,
		})
		if err != nil {
			t.Fatalf("add participant %d: %v", i, err)
		}
		if i < 3 {
			partsA = append(partsA, p)
		} else {
			partsB = append(partsB, p)
		}
	}

	// Re-fetch the challenge so the fixture has the open_registration row.
	tx, _ := uow.Query(context.Background())
	freshCh, err := chRepo.GetByID(tx, ch.ID)
	if err != nil {
		t.Fatalf("re-fetch challenge: %v", err)
	}

	return &challengesFixture{
		t:          t,
		db:         db,
		uow:        uow,
		recorder:   recorder,
		gymID:      owner.GymID,
		ownerID:    owner.UserID,
		challenge:  freshCh,
		catA:       catA,
		catB:       catB,
		partsA:     partsA,
		partsB:     partsB,
		chRepo:     chRepo,
		catRepo:    catRepo,
		partRepo:   partRepo,
		mRepo:      mRepo,
		attendance: attendance,
	}
}

// captureT0 / captureT1 are separate so tests can sequence ALL T₀s before
// transitioning the challenge into measuring_t1 — the state-machine window
// predicates only allow T₀ before that transition and T₁ after it.
func (f *challengesFixture) captureT0(p *participantDomain.Participant, in measurementInput) {
	f.t.Helper()
	at := f.challenge.StartsAt.Add(time.Hour)
	cap := challengesApp.NewCaptureMeasurement(f.chRepo, f.partRepo, f.mRepo, f.uow, f.recorder)
	cap.NowFunc = func() time.Time { return at }
	if _, err := cap.Execute(context.Background(), challengesApp.CaptureMeasurementInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ChallengeID: f.challenge.ID, ParticipantID: p.ID,
		Input: in.toDomain(measurementDomain.MomentT0, at, f.ownerID),
	}); err != nil {
		f.t.Fatalf("capture T0: %v", err)
	}
}

func (f *challengesFixture) captureT1(p *participantDomain.Participant, in measurementInput) {
	f.t.Helper()
	at := f.challenge.MeasurementT1Start.Add(time.Hour)
	cap := challengesApp.NewCaptureMeasurement(f.chRepo, f.partRepo, f.mRepo, f.uow, f.recorder)
	cap.NowFunc = func() time.Time { return at }
	if _, err := cap.Execute(context.Background(), challengesApp.CaptureMeasurementInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ChallengeID: f.challenge.ID, ParticipantID: p.ID,
		Input: in.toDomain(measurementDomain.MomentT1, at, f.ownerID),
	}); err != nil {
		f.t.Fatalf("capture T1: %v", err)
	}
}

// transitionToMeasuringT1 advances the challenge into the T₁ capture window.
// Idempotent — once measuring_t1, additional calls are silently no-ops.
func (f *challengesFixture) transitionToMeasuringT1() {
	f.t.Helper()
	at := f.challenge.MeasurementT1Start
	tr := challengesApp.NewTransitionChallengeStatus(f.chRepo, f.catRepo, f.uow, f.recorder)
	tr.NowFunc = func() time.Time { return at }
	_, _ = tr.Execute(context.Background(), challengesApp.TransitionChallengeStatusInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ChallengeID: f.challenge.ID, Transition: challengesApp.TransitionStartRunning,
	})
	_, _ = tr.Execute(context.Background(), challengesApp.TransitionChallengeStatusInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ChallengeID: f.challenge.ID, Transition: challengesApp.TransitionStartMeasuringT1,
	})
}

type measurementInput struct {
	BodyWeight float64
	BodyFat    float64
	LegsW      float64
	LegsR      int
	PushW      float64
	PushR      int
	PullW      float64
	PullR      int
}

func (m measurementInput) toDomain(moment string, at time.Time, user uuid.UUID) measurementDomain.Input {
	return measurementDomain.Input{
		Moment:          moment,
		MeasuredAt:      at,
		BodyWeightKg:    m.BodyWeight,
		BodyFatPct:      m.BodyFat,
		LegsWeightKg:    m.LegsW,
		LegsReps:        m.LegsR,
		PushWeightKg:    m.PushW,
		PushReps:        m.PushR,
		PullWeightKg:    m.PullW,
		PullReps:        m.PullR,
		CreatedByUserID: user,
	}
}

// ─── Test 1 — Obvious ranking ────────────────────────────────────────────

func TestIntegration_Ranking_ObviousOrder(t *testing.T) {
	f := setupChallenges(t)
	// All 6 participants get T₀ before the challenge advances, then T₁ after.
	// Differences: progressively bigger fat loss and muscle gain → clearly
	// ordered IR.
	for i := 0; i < 3; i++ {
		f.captureT0(f.partsA[i], measurementInput{BodyWeight: 80, BodyFat: 22, LegsW: 80, LegsR: 5, PushW: 60, PushR: 5, PullW: 100, PullR: 3})
		f.captureT0(f.partsB[i], measurementInput{BodyWeight: 65, BodyFat: 30, LegsW: 50, LegsR: 5, PushW: 35, PushR: 5, PullW: 60, PullR: 3})
	}
	f.transitionToMeasuringT1()
	for i := 0; i < 3; i++ {
		fatDelta := float64(2 + i)        // 2, 3, 4 percentage points
		liftDelta := float64(5 * (i + 1)) // 5, 10, 15 kg
		f.captureT1(f.partsA[i], measurementInput{BodyWeight: 80, BodyFat: 22 - fatDelta, LegsW: 80 + liftDelta, LegsR: 5, PushW: 60 + liftDelta, PushR: 5, PullW: 100 + liftDelta, PullR: 3})
		f.captureT1(f.partsB[i], measurementInput{BodyWeight: 65, BodyFat: 30 - fatDelta, LegsW: 50 + liftDelta, LegsR: 5, PushW: 35 + liftDelta, PushR: 5, PullW: 60 + liftDelta, PullR: 3})
	}

	ranking := challengesApp.NewGetChallengeRanking(f.chRepo, f.partRepo, f.mRepo, f.attendance, f.uow)
	ranking.NowFunc = func() time.Time { return f.challenge.MeasurementT1Start.Add(2 * time.Hour) }
	entries, err := ranking.Execute(context.Background(), challengesApp.GetChallengeRankingInput{
		GymID: f.gymID, ChallengeID: f.challenge.ID, CategoryID: &f.catA.ID,
	})
	if err != nil {
		t.Fatalf("ranking: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(catA ranking) = %d, want 3", len(entries))
	}
	// participant index 2 has the biggest deltas → IR should be #1.
	if entries[0].ParticipantID != f.partsA[2].ID {
		t.Errorf("#1 = %v, want partsA[2] (%v)", entries[0].ParticipantID, f.partsA[2].ID)
	}
	if entries[2].ParticipantID != f.partsA[0].ID {
		t.Errorf("#3 = %v, want partsA[0] (%v)", entries[2].ParticipantID, f.partsA[0].ID)
	}
	if entries[0].Position != 1 || entries[1].Position != 2 || entries[2].Position != 3 {
		t.Errorf("positions wrong: %v", []int{entries[0].Position, entries[1].Position, entries[2].Position})
	}
}

// ─── Test 2 — Technical tie ──────────────────────────────────────────────

func TestIntegration_Ranking_TechnicalTie(t *testing.T) {
	f := setupChallenges(t)
	// Two participants in catA with effectively identical IRs (well inside
	// the default TieMarginIR=5). Third one is much worse.
	t0 := measurementInput{BodyWeight: 80, BodyFat: 22, LegsW: 80, LegsR: 5, PushW: 60, PushR: 5, PullW: 100, PullR: 3}
	f.captureT0(f.partsA[0], t0)
	f.captureT0(f.partsA[1], t0)
	f.captureT0(f.partsA[2], t0)
	f.transitionToMeasuringT1()
	f.captureT1(f.partsA[0], measurementInput{BodyWeight: 80, BodyFat: 19, LegsW: 85, LegsR: 5, PushW: 63, PushR: 5, PullW: 103, PullR: 3})
	f.captureT1(f.partsA[1], measurementInput{BodyWeight: 80, BodyFat: 19, LegsW: 86, LegsR: 5, PushW: 64, PushR: 5, PullW: 104, PullR: 3})
	f.captureT1(f.partsA[2], measurementInput{BodyWeight: 80, BodyFat: 21.5, LegsW: 80, LegsR: 5, PushW: 60, PushR: 5, PullW: 100, PullR: 3})

	ranking := challengesApp.NewGetChallengeRanking(f.chRepo, f.partRepo, f.mRepo, f.attendance, f.uow)
	ranking.NowFunc = func() time.Time { return f.challenge.MeasurementT1Start.Add(2 * time.Hour) }
	entries, err := ranking.Execute(context.Background(), challengesApp.GetChallengeRankingInput{
		GymID: f.gymID, ChallengeID: f.challenge.ID, CategoryID: &f.catA.ID,
	})
	if err != nil {
		t.Fatalf("ranking: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if !entries[0].Tied || !entries[1].Tied {
		t.Errorf("expected top two to be tied; got tied=%v %v %v", entries[0].Tied, entries[1].Tied, entries[2].Tied)
	}
	if entries[2].Tied {
		t.Errorf("third place should not be flagged tied")
	}
}

// ─── Test 3 — Missing T₁ ─────────────────────────────────────────────────

func TestIntegration_Ranking_ExcludesMissingT1(t *testing.T) {
	f := setupChallenges(t)
	t0 := measurementInput{BodyWeight: 80, BodyFat: 22, LegsW: 80, LegsR: 5, PushW: 60, PushR: 5, PullW: 100, PullR: 3}
	// Both T₀s captured up front; only partsA[0] gets a T₁ later.
	f.captureT0(f.partsA[0], t0)
	f.captureT0(f.partsA[1], t0)
	f.transitionToMeasuringT1()
	f.captureT1(f.partsA[0], measurementInput{BodyWeight: 80, BodyFat: 20, LegsW: 85, LegsR: 5, PushW: 63, PushR: 5, PullW: 103, PullR: 3})

	ranking := challengesApp.NewGetChallengeRanking(f.chRepo, f.partRepo, f.mRepo, f.attendance, f.uow)
	ranking.NowFunc = func() time.Time { return f.challenge.MeasurementT1Start.Add(2 * time.Hour) }
	entries, err := ranking.Execute(context.Background(), challengesApp.GetChallengeRankingInput{
		GymID: f.gymID, ChallengeID: f.challenge.ID, CategoryID: &f.catA.ID,
	})
	if err != nil {
		t.Fatalf("ranking: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("entries = %d, want 1 (T₁ missing for partsA[1])", len(entries))
	}
	if len(entries) == 1 && entries[0].ParticipantID != f.partsA[0].ID {
		t.Errorf("ranked participant = %v, want partsA[0]", entries[0].ParticipantID)
	}
}

// ─── Test 4 — Attendance insufficient ────────────────────────────────────

func TestIntegration_Ranking_AttendanceInsufficient(t *testing.T) {
	f := setupChallenges(t)
	f.captureT0(f.partsA[0], measurementInput{BodyWeight: 80, BodyFat: 22, LegsW: 80, LegsR: 5, PushW: 60, PushR: 5, PullW: 100, PullR: 3})
	f.transitionToMeasuringT1()
	f.captureT1(f.partsA[0], measurementInput{BodyWeight: 80, BodyFat: 20, LegsW: 85, LegsR: 5, PushW: 63, PushR: 5, PullW: 103, PullR: 3})
	// Insert a single check-in for that participant's member — far below the
	// 3-per-week minimum across multiple weeks. The fixture's challenge has
	// run for over a week by `now`, so the participant should be flagged.
	if _, err := f.db.Exec(
		`INSERT INTO checkins (id, gym_id, version, created_at, updated_at, member_id, checkin_at, method, result)
		 VALUES (?, ?, 1, ?, ?, ?, ?, 'manual', 'allowed_active')`,
		uuid.New().String(), f.gymID.String(),
		f.challenge.StartsAt.UnixMilli(), f.challenge.StartsAt.UnixMilli(),
		f.partsA[0].MemberID.String(),
		f.challenge.StartsAt.Add(2*24*time.Hour).UnixMilli(),
	); err != nil {
		t.Fatalf("insert checkin: %v", err)
	}

	ranking := challengesApp.NewGetChallengeRanking(f.chRepo, f.partRepo, f.mRepo, f.attendance, f.uow)
	ranking.NowFunc = func() time.Time { return f.challenge.MeasurementT1Start.Add(2 * time.Hour) }
	entries, err := ranking.Execute(context.Background(), challengesApp.GetChallengeRankingInput{
		GymID: f.gymID, ChallengeID: f.challenge.ID, CategoryID: &f.catA.ID,
	})
	if err != nil {
		t.Fatalf("ranking: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if !entries[0].AttendanceInsufficient {
		t.Errorf("expected AttendanceInsufficient=true with only one check-in across the whole window")
	}
}

// ─── Test 5 — Config locked after a measurement ──────────────────────────

func TestIntegration_UpdateConfig_LockedAfterMeasurement(t *testing.T) {
	f := setupChallenges(t)
	// One T₀ capture is enough to lock config edits.
	t0Now := f.challenge.StartsAt.Add(time.Hour)
	cap := challengesApp.NewCaptureMeasurement(f.chRepo, f.partRepo, f.mRepo, f.uow, f.recorder)
	cap.NowFunc = func() time.Time { return t0Now }
	if _, err := cap.Execute(context.Background(), challengesApp.CaptureMeasurementInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ChallengeID: f.challenge.ID, ParticipantID: f.partsA[0].ID,
		Input: measurementInput{BodyWeight: 80, BodyFat: 22, LegsW: 80, LegsR: 5, PushW: 60, PushR: 5, PullW: 100, PullR: 3}.
			toDomain(measurementDomain.MomentT0, t0Now, f.ownerID),
	}); err != nil {
		t.Fatalf("capture: %v", err)
	}

	update := challengesApp.NewUpdateChallengeConfig(f.chRepo, f.mRepo, f.uow, f.recorder)
	update.NowFunc = func() time.Time { return t0Now.Add(time.Hour) }
	cap2 := 30.0
	_, err := update.Execute(context.Background(), challengesApp.UpdateChallengeConfigInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ChallengeID: f.challenge.ID,
		Config:      challengeDomain.ConfigUpdate{StrengthCapPct: &cap2},
	})
	if err == nil {
		t.Fatalf("expected ErrConfigLocked after a measurement")
	}
}

// ─── Test 6 — Supersession persists both rows ────────────────────────────

func TestIntegration_Supersession_PersistsBothRows(t *testing.T) {
	f := setupChallenges(t)
	t0Now := f.challenge.StartsAt.Add(time.Hour)
	cap := challengesApp.NewCaptureMeasurement(f.chRepo, f.partRepo, f.mRepo, f.uow, f.recorder)
	cap.NowFunc = func() time.Time { return t0Now }

	first, err := cap.Execute(context.Background(), challengesApp.CaptureMeasurementInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ChallengeID: f.challenge.ID, ParticipantID: f.partsA[0].ID,
		Input: measurementInput{BodyWeight: 80, BodyFat: 22, LegsW: 80, LegsR: 5, PushW: 60, PushR: 5, PullW: 100, PullR: 3}.
			toDomain(measurementDomain.MomentT0, t0Now, f.ownerID),
	})
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	cap.NowFunc = func() time.Time { return t0Now.Add(time.Minute) }
	second, err := cap.Execute(context.Background(), challengesApp.CaptureMeasurementInput{
		GymID: f.gymID, ActorUserID: f.ownerID,
		ChallengeID: f.challenge.ID, ParticipantID: f.partsA[0].ID,
		Input: measurementInput{BodyWeight: 81, BodyFat: 22, LegsW: 80, LegsR: 5, PushW: 60, PushR: 5, PullW: 100, PullR: 3}.
			toDomain(measurementDomain.MomentT0, t0Now.Add(time.Minute), f.ownerID),
	})
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if second.SupersededPriorID == nil || *second.SupersededPriorID != first.Measurement.ID {
		t.Errorf("second.SupersededPriorID = %v, want %v", second.SupersededPriorID, first.Measurement.ID)
	}

	tx, _ := f.uow.Query(context.Background())
	all, err := f.mRepo.ListByParticipant(tx, f.partsA[0].ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListByParticipant = %d, want 2", len(all))
	}
	active, ok, err := f.mRepo.GetActiveByMoment(tx, f.partsA[0].ID, measurementDomain.MomentT0)
	if err != nil {
		t.Fatalf("active query: %v", err)
	}
	if !ok || active.ID != second.Measurement.ID {
		t.Errorf("active = %v, want second %v", active, second.Measurement.ID)
	}
}
