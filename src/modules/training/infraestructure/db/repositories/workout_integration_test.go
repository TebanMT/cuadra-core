//go:build server && integration

package repositories

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	"github.com/cuadra/cuadra-core/src/modules/training/infraestructure/timezone"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
)

func workoutDependencies(base trainingApp.CreatePersonalExerciseDependencies) trainingApp.StartWorkoutDependencies {
	return trainingApp.StartWorkoutDependencies{UoW: base.UoW, Versions: NewRoutinePublicationPostgresRepository(), Workouts: NewWorkoutPostgresRepository(), Commands: base.Commands, Canonicalizer: base.Canonicalizer, Timezones: timezone.NewIANA(), Now: base.Now}
}
func workoutUseCase(t *testing.T, d trainingApp.StartWorkoutDependencies) *trainingApp.StartWorkout {
	t.Helper()
	u, err := trainingApp.NewStartWorkout(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func publishedWorkoutFixture(t *testing.T, base trainingApp.CreatePersonalExerciseDependencies, a security.AccessClaims) *routine.Version {
	t.Helper()
	c := completeRoutine(t, base, a)
	d := publicationDependencies(base)
	p := publicationCommand(a, c, 1, 1)
	requirePublicationOutcome(t, publicationUseCase(t, d), a, p, "accepted", "")
	v, err := readPublishedVersion(t, d, a.AccountID, uuid.MustParse(p.Payload.VersionID))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func workoutCommand(a security.AccessClaims, v *routine.Version) workout.StartCommand {
	p := workout.StartPayload{RoutineVersionID: v.ID.String(), RoutineDayID: v.Days[0].ID.String(), VersionHash: hex.EncodeToString(v.ContentHash[:]), StartedAt: "2020-11-01T05:30:00Z", Timezone: "America/New_York", Exercises: []workout.ExerciseMap{}}
	for _, e := range v.Days[0].Exercises {
		p.Exercises = append(p.Exercises, workout.ExerciseMap{SourceExerciseID: e.ID.String(), ID: uuid.NewString()})
	}
	return workout.StartCommand{OperationID: uuid.NewString(), OriginDeviceID: a.DeviceID.String(), EntityID: uuid.NewString(), Type: workout.StartCommandType, Base: protocol.Base{Revision: ptr("0")}, CapturedAt: "2099-10-19T10:00:00Z", Payload: p}
}
func cloneWorkoutCommand(t *testing.T, c workout.StartCommand) workout.StartCommand {
	t.Helper()
	raw, _ := json.Marshal(c)
	var out workout.StartCommand
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func requireWorkoutOutcome(t *testing.T, u *trainingApp.StartWorkout, a security.AccessClaims, c workout.StartCommand, outcome, code string) protocol.OperationResult {
	t.Helper()
	got, err := u.Execute(context.Background(), a, c)
	if err != nil || got.Outcome != outcome || code != "" && (got.Error == nil || got.Error.Code != code) {
		t.Fatal("unexpected workout outcome", got.Outcome, err)
	}
	return got
}
func readWorkout(t *testing.T, d trainingApp.StartWorkoutDependencies, owner, id uuid.UUID) (*workout.Workout, error) {
	t.Helper()
	tx, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), owner))
	if err != nil {
		return nil, err
	}
	defer shared.CloseQuery(tx)
	return d.Workouts.GetByID(tx, owner, id)
}
func TestWorkoutStartPostgresReplayCompleteSnapshotAndIndependentIdentities(t *testing.T) {
	owner, a, base := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	d := workoutDependencies(base)
	u := workoutUseCase(t, d)
	c := workoutCommand(a, v)
	c.Payload.Exercises[0], c.Payload.Exercises[1] = c.Payload.Exercises[1], c.Payload.Exercises[0]
	const n = 5
	results := make([]protocol.OperationResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = u.Execute(context.Background(), a, c) }(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Outcome != "accepted" || !reflect.DeepEqual(results[i], results[0]) {
			t.Fatal("concurrent replay changed result", errs[i])
		}
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE account_id=?", a.AccountID) != 1 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workout_sets WHERE account_id=?", a.AccountID) != 0 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", c.OperationID) != 1 {
		t.Fatal("duplicate or fabricated results")
	}
	w, err := readWorkout(t, d, a.AccountID, uuid.MustParse(c.EntityID))
	if err != nil || !w.MatchesVersion(v) || w.Revision != 1 || w.State != "in_progress" || w.NeedsReview || w.FinalizedRevision != nil {
		t.Fatal("invalid persisted snapshot", err)
	}
	var raw string
	if err = owner.Raw("SELECT changes::text FROM mancuerna.sync_changes WHERE account_id=? AND operation_id=?", a.AccountID, c.OperationID).Row().Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var changes []protocol.Change
	if json.Unmarshal([]byte(raw), &changes) != nil || len(changes) != 1 || changes[0].EntityType != "workout" || changes[0].Revision != "1" {
		t.Fatal("workout projection not indivisible")
	}
	expected, _ := json.Marshal(w.DTO())
	want, _ := base.Canonicalizer.Canonicalize(expected)
	got, _ := base.Canonicalizer.Canonicalize(changes[0].Data)
	if string(want) != string(got) {
		t.Fatal("projection differs from sealed database")
	}
	altered := cloneWorkoutCommand(t, c)
	altered.CapturedAt = "2099-10-19T10:00:00.000Z"
	if _, err = u.Execute(context.Background(), a, altered); !errors.Is(err, protocol.ErrOperationMismatch) {
		t.Fatal("changed capture text accepted", err)
	}
	other := workoutCommand(a, v)
	requireWorkoutOutcome(t, u, a, other, "accepted", "")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE account_id=? AND state='in_progress'", a.AccountID) != 2 {
		t.Fatal("independent training sessions deduplicated")
	}
}
func TestWorkoutStartPostgresHistoricalVersionAndMaterialValidation(t *testing.T) {
	owner, a, base := trainingFixture(t)
	v1 := publishedWorkoutFixture(t, base, a)
	d := workoutDependencies(base)
	u := workoutUseCase(t, d)
	first := workoutCommand(a, v1)
	requireWorkoutOutcome(t, u, a, first, "accepted", "")
	before, err := readWorkout(t, d, a.AccountID, uuid.MustParse(first.EntityID))
	if err != nil {
		t.Fatal(err)
	}
	bytesBefore, _ := json.Marshal(before.DTO())
	if err = owner.Exec("UPDATE mancuerna.personal_exercises SET name='Renamed',load_mode='none' WHERE account_id=? AND id=?", a.AccountID, v1.Days[0].Exercises[0].Reference.ID).Error; err != nil {
		t.Fatal(err)
	}
	edit := editFromVersion(t, base, a, v1, 2)
	p := publicationCommand(a, edit, 2, 3)
	pd := publicationDependencies(base)
	requirePublicationOutcome(t, publicationUseCase(t, pd), a, p, "accepted", "")
	for _, sql := range []string{"UPDATE mancuerna.routines SET status='archived',deleted_at=clock_timestamp() WHERE account_id=? AND id=?", "UPDATE mancuerna.personal_exercises SET archived_at=clock_timestamp(),deleted_at=clock_timestamp() WHERE account_id=? AND id=?"} {
		id := v1.RoutineID.String()
		if strings.Contains(sql, "personal_exercises") {
			id = v1.Days[0].Exercises[0].Reference.ID
		}
		if err = owner.Exec(sql, a.AccountID, id).Error; err != nil {
			t.Fatal(err)
		}
	}
	historical := workoutCommand(a, v1)
	requireWorkoutOutcome(t, u, a, historical, "accepted", "")
	w, err := readWorkout(t, d, a.AccountID, uuid.MustParse(historical.EntityID))
	if err != nil || !w.MatchesVersion(v1) {
		t.Fatal("historical version cannot start", err)
	}
	after, err := readWorkout(t, d, a.AccountID, uuid.MustParse(first.EntityID))
	if err != nil {
		t.Fatal(err)
	}
	bytesAfter, _ := json.Marshal(after.DTO())
	if string(bytesBefore) != string(bytesAfter) {
		t.Fatal("workout snapshot changed with library")
	}
	for _, mutate := range []func(*workout.StartCommand){func(c *workout.StartCommand) { c.Payload.VersionHash = strings.Repeat("0", 64) }, func(c *workout.StartCommand) { c.Payload.RoutineVersionID = uuid.NewString() }, func(c *workout.StartCommand) { c.Payload.RoutineDayID = v1.Days[1].ID.String() }, func(c *workout.StartCommand) { c.Payload.Exercises = c.Payload.Exercises[:1] }, func(c *workout.StartCommand) {
		c.Payload.Exercises[1].SourceExerciseID = c.Payload.Exercises[0].SourceExerciseID
	}, func(c *workout.StartCommand) { c.Payload.Exercises[1].ID = c.Payload.Exercises[0].ID }, func(c *workout.StartCommand) { c.Payload.Exercises[0].ID = c.Payload.Exercises[0].SourceExerciseID }, func(c *workout.StartCommand) { c.Base = protocol.Base{Revision: ptr("1")} }, func(c *workout.StartCommand) { c.Base = protocol.Base{AfterOperationID: ptr(p.OperationID)} }} {
		bad := workoutCommand(a, v1)
		mutate(&bad)
		rejected := requireWorkoutOutcome(t, u, a, bad, "rejected", "VALIDATION_ERROR")
		if replay := requireWorkoutOutcome(t, u, a, bad, "rejected", "VALIDATION_ERROR"); !reflect.DeepEqual(rejected, replay) {
			t.Fatal("rejection not durable")
		}
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE account_id=?", a.AccountID) != 2 {
		t.Fatal("invalid material wrote aggregate")
	}
}

type failAfterWorkout struct{ repository.WorkoutRepository }

func (r failAfterWorkout) Create(tx shared.Transaction, owner uuid.UUID, w *workout.Workout) (bool, error) {
	ok, err := r.WorkoutRepository.Create(tx, owner, w)
	if err == nil && ok {
		return false, errors.New("synthetic failure after sealed workout")
	}
	return ok, err
}
func TestWorkoutStartPostgresGlobalCollisionsAndAtomicRollback(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, baseB := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	vb := publishedWorkoutFixture(t, baseB, b)
	d := workoutDependencies(base)
	u := workoutUseCase(t, d)
	db := workoutDependencies(baseB)
	ub := workoutUseCase(t, db)
	old := workoutCommand(a, v)
	foreign := workoutCommand(b, vb)
	requireWorkoutOutcome(t, u, a, old, "accepted", "")
	requireWorkoutOutcome(t, ub, b, foreign, "accepted", "")
	for _, existing := range []workout.StartCommand{old, foreign} {
		for _, child := range []bool{false, true} {
			bad := workoutCommand(a, v)
			if child {
				bad.Payload.Exercises[0].ID = existing.Payload.Exercises[0].ID
			} else {
				bad.EntityID = existing.EntityID
			}
			before := trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE account_id=?", a.AccountID)
			requireWorkoutOutcome(t, u, a, bad, "rejected", "REVISION_CONFLICT")
			if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE account_id=?", a.AccountID) != before || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workout_exercises WHERE account_id=?", a.AccountID) != 2 {
				t.Fatal("collision escaped savepoint")
			}
		}
	}
	candidate := workoutCommand(a, v)
	sequence := trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID)
	for _, mode := range []int{0, 1} {
		broken := d
		if mode == 0 {
			broken.Workouts = failAfterWorkout{d.Workouts}
		} else {
			broken.Commands = trainingFailCommit{d.Commands}
		}
		if _, err := workoutUseCase(t, broken).Execute(context.Background(), a, candidate); err == nil {
			t.Fatal("injected failure ignored")
		}
		if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE id=?", candidate.EntityID) != 0 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workout_exercises WHERE workout_id=?", candidate.EntityID) != 0 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", candidate.OperationID) != 0 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != sequence {
			t.Fatal("partial workout/journal survived rollback")
		}
	}
	requireWorkoutOutcome(t, u, a, candidate, "accepted", "")
	// Two accounts collide on the same child UUID; neither reads its owner.
	ca, cb := workoutCommand(a, v), workoutCommand(b, vb)
	cb.Payload.Exercises[0].ID = ca.Payload.Exercises[0].ID
	var ra, rb protocol.OperationResult
	var ea, eb error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); ra, ea = u.Execute(context.Background(), a, ca) }()
	go func() { defer wg.Done(); rb, eb = ub.Execute(context.Background(), b, cb) }()
	wg.Wait()
	if ea != nil || eb != nil {
		t.Fatal("concurrent collision did not complete", ea, eb)
	}
	wins := 0
	for _, r := range []protocol.OperationResult{ra, rb} {
		if r.Outcome == "accepted" {
			wins++
		} else if r.Outcome != "rejected" || r.Error.Code != "REVISION_CONFLICT" {
			t.Fatal("unexpected concurrent collision result")
		}
	}
	if wins != 1 {
		t.Fatal("shared child accepted twice")
	}
}
func TestWorkoutStartPostgresScopeActorAndHistoricalOrigin(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	d := workoutDependencies(base)
	u := workoutUseCase(t, d)
	c := workoutCommand(a, v)
	foreign := cloneWorkoutCommand(t, c)
	foreign.OperationID = uuid.NewString()
	foreign.OriginDeviceID = b.DeviceID.String()
	requireWorkoutOutcome(t, u, b, foreign, "rejected", "VALIDATION_ERROR")
	for _, ctx := range []context.Context{shared.WithPersonalAccount(context.Background(), b.AccountID), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := u.Execute(ctx, a, c); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("ambient mixed scope accepted", err)
		}
	}
	auth, err := shared.NewPersonalPostgresUnitOfWork(trainingTestPool(t, "MANCUERNA_TEST_AUTH_DSN"), shared.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	wrong := d
	wrong.UoW = auth
	if _, err = workoutUseCase(t, wrong).Execute(context.Background(), a, c); err == nil {
		t.Fatal("auth pool wrote workout")
	}
	origin := uuid.New()
	if err = owner.Exec("INSERT INTO mancuerna.devices(id,account_id,platform,label,public_key,authorized_at,authorized_by_method,revoked_at) VALUES(?,?,'ios','Historical synthetic',?,clock_timestamp()-interval '1 day','signup',clock_timestamp())", origin, a.AccountID, make([]byte, 32)).Error; err != nil {
		t.Fatal(err)
	}
	historical := workoutCommand(a, v)
	historical.OriginDeviceID = origin.String()
	requireWorkoutOutcome(t, u, a, historical, "accepted", "")
	for _, badOrigin := range []string{uuid.NewString(), b.DeviceID.String()} {
		bad := workoutCommand(a, v)
		bad.OriginDeviceID = badOrigin
		if _, err = u.Execute(context.Background(), a, bad); !errors.Is(err, protocol.ErrCommandOrigin) {
			t.Fatal("foreign or absent origin accepted", err)
		}
	}
	requireWorkoutOutcome(t, u, a, c, "accepted", "")
	if _, err = readWorkout(t, d, b.AccountID, uuid.MustParse(c.EntityID)); !errors.Is(err, trainingErrors.ErrWorkoutNotFound) {
		t.Fatal("B read A workout", err)
	}
	if err = owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", a.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = u.Execute(context.Background(), a, c); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("revoked actor replayed workout", err)
	}
}
func TestWorkoutStartPostgresSealAndRepositoryBoundary(t *testing.T) {
	owner, a, base := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	d := workoutDependencies(base)
	c := workoutCommand(a, v)
	requireWorkoutOutcome(t, workoutUseCase(t, d), a, c, "accepted", "")
	ctx := shared.WithPersonalAccount(context.Background(), a.AccountID)
	for _, query := range []string{"UPDATE mancuerna.workouts SET snapshot_sealed=false WHERE id=?", "UPDATE mancuerna.workouts SET routine_title_snapshot='invented' WHERE id=?", "UPDATE mancuerna.workouts SET timezone='UTC' WHERE id=?", "UPDATE mancuerna.workouts SET started_at=clock_timestamp() WHERE id=?", "UPDATE mancuerna.workout_exercises SET notes='invented' WHERE workout_id=?", "DELETE FROM mancuerna.workout_exercises WHERE workout_id=?"} {
		err := d.UoW.Command(ctx, func(tx shared.Transaction) error {
			db, err := trainingDataConnection(tx, a.AccountID, true)
			if err != nil {
				return err
			}
			return db.Exec(query, c.EntityID).Error
		})
		if err == nil {
			t.Fatal("immutable snapshot modified")
		}
	}
	w, err := workout.NewStarted(uuid.New(), a.AccountID, a.DeviceID, c.Payload, v, base.Now())
	if err != nil {
		t.Fatal(err)
	}
	w.Exercises[0].Notes = "invented"
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error { _, err := d.Workouts.Create(tx, a.AccountID, w); return err })
	if !errors.Is(err, trainingErrors.ErrInvalidWorkout) {
		t.Fatal("repository accepted invented snapshot", err)
	}
	valid := workoutCommand(a, v)
	w, err = workout.NewStarted(uuid.MustParse(valid.EntityID), a.AccountID, a.DeviceID, valid.Payload, v, base.Now())
	if err != nil {
		t.Fatal(err)
	}
	root, children := workoutToModels(w)
	// Deferred database completeness guard rejects a transaction that seals only
	// one of the two required appearances, even with valid foreign keys.
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		db, err := trainingDataConnection(tx, a.AccountID, true)
		if err != nil {
			return err
		}
		if err = db.Create(&root).Error; err != nil {
			return err
		}
		if err = db.Create(&children[0]).Error; err != nil {
			return err
		}
		return db.Exec("UPDATE mancuerna.workouts SET snapshot_sealed=true WHERE id=?", root.ID).Error
	})
	if err == nil {
		t.Fatal("partial sealed workout committed")
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE id=?", valid.EntityID) != 0 {
		t.Fatal("incomplete aggregate remains")
	}
	// A late insert cannot extend an existing immutable snapshot.
	children[0].ID = uuid.New()
	children[0].WorkoutID = uuid.MustParse(c.EntityID)
	err = d.UoW.Command(ctx, func(tx shared.Transaction) error {
		db, err := trainingDataConnection(tx, a.AccountID, true)
		if err != nil {
			return err
		}
		return db.Create(&children[0]).Error
	})
	if err == nil {
		t.Fatal("late child inserted")
	}
}
func TestWorkoutStartPostgresTimezoneAndOriginalCapture(t *testing.T) {
	owner, a, base := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	d := workoutDependencies(base)
	u := workoutUseCase(t, d)
	for _, zone := range []string{"Local", "localtime", "posixrules", "+06:00", "America/Invented", " UTC"} {
		c := workoutCommand(a, v)
		c.Payload.Timezone = zone
		requireWorkoutOutcome(t, u, a, c, "rejected", "VALIDATION_ERROR")
	}
	for _, at := range []string{"2020-11-01T05:30:00Z", "2020-11-01T06:30:00.000Z", "2099-01-01T00:00:00Z", "0001-01-01T00:00:00Z"} {
		c := workoutCommand(a, v)
		c.Payload.StartedAt = at
		requireWorkoutOutcome(t, u, a, c, "accepted", "")
		w, err := readWorkout(t, d, a.AccountID, uuid.MustParse(c.EntityID))
		expected, _ := time.Parse(time.RFC3339Nano, at)
		if err != nil || !w.StartedAt.Equal(expected) || w.Timezone != c.Payload.Timezone || w.CreatedAt.Equal(w.StartedAt) {
			t.Fatal("client time moved or used as server time", err)
		}
		var captured string
		if err = owner.Raw("SELECT captured_at FROM mancuerna.operation_receipts WHERE operation_id=?", c.OperationID).Row().Scan(&captured); err != nil || captured != c.CapturedAt {
			t.Fatal("original capture metadata not stored", err)
		}
	}
}

type denyWorkoutChanges struct{ protocolRepository.Commands }

func (r denyWorkoutChanges) CheckChanges(shared.Transaction, uuid.UUID, uuid.UUID, []protocol.Change) error {
	return protocol.ErrCommandLimit
}
func TestWorkoutStartPostgresMaximumSnapshotAndPreflightLimit(t *testing.T) {
	owner, a, base := trainingFixture(t)
	exercise := trainingCommand(a)
	exercise.Payload.Name = strings.Repeat("🙂", 120)
	require := trainingUseCase(t, base)
	if got, err := require.Execute(context.Background(), a, exercise); err != nil || got.Outcome != "accepted" {
		t.Fatal("max exercise prerequisite", err)
	}
	c := routineCommand(a)
	c.Payload.Draft.Title = strings.Repeat("🙂", 120)
	day := routine.DraftDay{ID: uuid.NewString(), Label: strings.Repeat("🙂", 80), Exercises: []routine.DraftExercise{}}
	for i := 0; i < 50; i++ {
		sets, rest := 100, 3600
		ref := routine.ExerciseRef{Source: "personal", ID: exercise.EntityID}
		day.Exercises = append(day.Exercises, routine.DraftExercise{ID: uuid.NewString(), ExerciseRef: &ref, PrescribedSets: &sets, PrescribedReps: strings.Repeat("🙂", 80), RestSeconds: &rest, Notes: strings.Repeat("🙂", 1000)})
	}
	c.Payload.Draft.Days = append(c.Payload.Draft.Days, day)
	createDraftFixture(t, base, a, c)
	pd := publicationDependencies(base)
	p := publicationCommand(a, c, 1, 1)
	requirePublicationOutcome(t, publicationUseCase(t, pd), a, p, "accepted", "")
	v, err := readPublishedVersion(t, pd, a.AccountID, uuid.MustParse(p.Payload.VersionID))
	if err != nil {
		t.Fatal(err)
	}
	d := workoutDependencies(base)
	u := workoutUseCase(t, d)
	command := workoutCommand(a, v)
	requireWorkoutOutcome(t, u, a, command, "accepted", "")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workout_exercises WHERE workout_id=?", command.EntityID) != 50 || trainingCount(t, owner, "SELECT octet_length(changes::text) FROM mancuerna.sync_changes WHERE operation_id=?", command.OperationID) >= 1048576 {
		t.Fatal("maximum contractual snapshot exceeded journal bound")
	}
	// At 50 appearances the real maximum valid DTO is below 1 MiB. This injected
	// preflight error tests terminal mapping and absence of writes, not a claim
	// that a valid maximum workout can reach the shared group's size boundary.
	denied := d
	denied.Commands = denyWorkoutChanges{d.Commands}
	bad := workoutCommand(a, v)
	requireWorkoutOutcome(t, workoutUseCase(t, denied), a, bad, "rejected", "LIMIT_EXCEEDED")
	requireWorkoutOutcome(t, u, a, bad, "rejected", "LIMIT_EXCEEDED")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workouts WHERE id=?", bad.EntityID) != 0 {
		t.Fatal("limit rejection wrote workout")
	}
}
