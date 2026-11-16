//go:build server && integration

package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func setDependencies(base trainingApp.CreatePersonalExerciseDependencies) trainingApp.CreateWorkoutSetDependencies {
	return trainingApp.CreateWorkoutSetDependencies{UoW: base.UoW, Workouts: NewWorkoutPostgresRepository(), Sets: NewWorkoutSetPostgresRepository(), Commands: base.Commands, Canonicalizer: base.Canonicalizer, Now: base.Now}
}
func setUseCase(t *testing.T, d trainingApp.CreateWorkoutSetDependencies) *trainingApp.CreateWorkoutSet {
	t.Helper()
	u, err := trainingApp.NewCreateWorkoutSet(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func startSetWorkout(t *testing.T, base trainingApp.CreatePersonalExerciseDependencies, a security.AccessClaims, v *routine.Version) workout.StartCommand {
	t.Helper()
	c := workoutCommand(a, v)
	requireWorkoutOutcome(t, workoutUseCase(t, workoutDependencies(base)), a, c, "accepted", "")
	return c
}
func setCommand(a security.AccessClaims, w workout.StartCommand) workout.CreateSetCommand {
	return workout.CreateSetCommand{OperationID: uuid.NewString(), OriginDeviceID: a.DeviceID.String(), Type: workout.CreateSetCommandType, EntityID: uuid.NewString(), Base: protocol.Base{Revision: ptr("0")}, CapturedAt: "2099-10-19T10:00:00Z", Payload: workout.CreateSetPayload{WorkoutID: w.EntityID, WorkoutExerciseID: w.Payload.Exercises[0].ID, ObservedWorkout: protocol.Base{Revision: ptr("1")}, PositionHint: 0, Load: workout.LoadInput{LoadMode: "external", WeightValue: ptr("30.0"), WeightUnit: ptr("kg")}, Repetitions: 0}}
}
func requireSetOutcome(t *testing.T, u *trainingApp.CreateWorkoutSet, a security.AccessClaims, c workout.CreateSetCommand, outcome, code string) protocol.OperationResult {
	t.Helper()
	r, err := u.Execute(context.Background(), a, c)
	if err != nil || r.Outcome != outcome || code != "" && (r.Error == nil || r.Error.Code != code) {
		t.Fatal("unexpected set outcome", r.Outcome, err)
	}
	return r
}
func readSet(t *testing.T, d trainingApp.CreateWorkoutSetDependencies, owner, id uuid.UUID) (*workout.Set, error) {
	t.Helper()
	tx, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), owner))
	if err != nil {
		return nil, err
	}
	defer shared.CloseQuery(tx)
	return d.Sets.GetByID(tx, owner, id)
}
func TestWorkoutSetCreatePostgresExactLoadsAndHistoricalSnapshots(t *testing.T) {
	owner, a, base := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	w := startSetWorkout(t, base, a, v)
	d := setDependencies(base)
	u := setUseCase(t, d)
	for _, values := range [][3]string{{"30.0", "kg", "30.000"}, {"0", "lb", "0.000"}, {"9999999.999", "kg", "9999999.999"}} {
		c := setCommand(a, w)
		c.Payload.Load.WeightValue = ptr(values[0])
		c.Payload.Load.WeightUnit = ptr(values[1])
		result := requireSetOutcome(t, u, a, c, "accepted", "")
		s, err := readSet(t, d, a.AccountID, uuid.MustParse(c.EntityID))
		if err != nil || *s.Load.WeightValue != values[2] || *s.Load.WeightUnit != values[1] || s.Repetitions != 0 || s.Revision != 1 || s.CapturedAt.Year() != 2099 {
			t.Fatal("exact load/capture changed", err)
		}
		if len(result.Affected) != 2 || result.Affected[0].EntityType != "workout_set" || result.Affected[1].EntityType != "workout" {
			t.Fatal("missing coupled revisions")
		}
		var text string
		if err = owner.Raw("SELECT captured_at FROM mancuerna.operation_receipts WHERE operation_id=?", c.OperationID).Row().Scan(&text); err != nil || text != c.CapturedAt {
			t.Fatal("capture text lost", err)
		}
	}
	if err := owner.Exec("UPDATE mancuerna.accounts SET weight_unit='lb' WHERE id=?", a.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if err := owner.Exec("UPDATE mancuerna.personal_exercises SET load_mode='none',name='Changed' WHERE account_id=? AND id=?", a.AccountID, v.Days[0].Exercises[0].Reference.ID).Error; err != nil {
		t.Fatal(err)
	}
	historical := setCommand(a, w)
	requireSetOutcome(t, u, a, historical, "accepted", "")
	bad := setCommand(a, w)
	bad.Payload.Load = workout.LoadInput{LoadMode: "none"}
	requireSetOutcome(t, u, a, bad, "rejected", "VALIDATION_ERROR")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workout_sets WHERE account_id=? AND weight_unit='kg'", a.AccountID) != 3 {
		t.Fatal("profile reinterpreted stored units")
	}
	// A second real publication starts with a no-load exercise; no SQL snapshot
	// mutation is used to fabricate this branch.
	ex := trainingCommand(a)
	ex.Payload.LoadMode = exercise.None
	if r, err := trainingUseCase(t, base).Execute(context.Background(), a, ex); err != nil || r.Outcome != "accepted" {
		t.Fatal("none prerequisite", err)
	}
	rc := routineCommand(a)
	sets := 3
	ref := routine.ExerciseRef{Source: "personal", ID: ex.EntityID}
	rc.Payload.Draft.Title = "Bodyweight"
	rc.Payload.Draft.Days = []routine.DraftDay{{ID: uuid.NewString(), Label: "Day", Exercises: []routine.DraftExercise{{ID: uuid.NewString(), ExerciseRef: &ref, PrescribedSets: &sets, PrescribedReps: "8", Notes: ""}}}}
	createDraftFixture(t, base, a, rc)
	pd := publicationDependencies(base)
	p := publicationCommand(a, rc, 1, 1)
	requirePublicationOutcome(t, publicationUseCase(t, pd), a, p, "accepted", "")
	nv, err := readPublishedVersion(t, pd, a.AccountID, uuid.MustParse(p.Payload.VersionID))
	if err != nil {
		t.Fatal(err)
	}
	nw := startSetWorkout(t, base, a, nv)
	none := setCommand(a, nw)
	none.Payload.Load = workout.LoadInput{LoadMode: "none"}
	requireSetOutcome(t, u, a, none, "accepted", "")
	s, err := readSet(t, d, a.AccountID, uuid.MustParse(none.EntityID))
	if err != nil || s.Load.WeightValue != nil || s.Load.WeightUnit != nil {
		t.Fatal("none became fictitious zero", err)
	}
	requireSetOutcome(t, u, a, setCommand(a, nw), "rejected", "VALIDATION_ERROR")
}
func TestWorkoutSetCreatePostgresReplayConcurrentIndependentSets(t *testing.T) {
	owner, a, base := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	w := startSetWorkout(t, base, a, v)
	d := setDependencies(base)
	u := setUseCase(t, d)
	c := setCommand(a, w)
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
			t.Fatal("replay diverged", errs[i])
		}
	}
	changed := c
	changed.Payload.Load.WeightValue = ptr("30.000")
	if _, err := u.Execute(context.Background(), a, changed); !errors.Is(err, protocol.ErrOperationMismatch) {
		t.Fatal("original weight text not bound", err)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fresh := setCommand(a, w)
			results[i], errs[i] = u.Execute(context.Background(), a, fresh)
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Outcome != "accepted" {
			t.Fatal("independent equal series lost", errs[i])
		}
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workout_sets WHERE workout_id=? AND position_hint=0", w.EntityID) != n+1 || trainingCount(t, owner, "SELECT revision FROM mancuerna.workouts WHERE id=?", w.EntityID) != n+2 {
		t.Fatal("equal values or positions deduplicated")
	}
	var raw string
	if err := owner.Raw("SELECT changes::text FROM mancuerna.sync_changes WHERE operation_id=?", c.OperationID).Row().Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var changes []protocol.Change
	if json.Unmarshal([]byte(raw), &changes) != nil || len(changes) != 2 || changes[0].EntityType != "workout_set" || changes[1].EntityType != "workout" {
		t.Fatal("coupled projections split")
	}
}
func TestWorkoutSetCreatePostgresObservedDependenciesRemainExact(t *testing.T) {
	owner, a, base := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	d := setDependencies(base)
	u := setUseCase(t, d)
	parent := workoutCommand(a, v)
	child := setCommand(a, parent)
	child.Payload.ObservedWorkout = protocol.Base{AfterOperationID: ptr(parent.OperationID)}
	requireSetOutcome(t, u, a, child, "dependency_pending", "DEPENDENCY_PENDING")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", child.OperationID) != 0 {
		t.Fatal("pending consumed operation")
	}
	requireWorkoutOutcome(t, workoutUseCase(t, workoutDependencies(base)), a, parent, "accepted", "")
	first := requireSetOutcome(t, u, a, child, "accepted", "")
	if first.Affected[1].Revision != "2" {
		t.Fatal("incorrect parent resolution")
	}
	next := setCommand(a, parent)
	next.Payload.ObservedWorkout = protocol.Base{AfterOperationID: ptr(child.OperationID)}
	requireSetOutcome(t, u, a, next, "accepted", "")
	// That receipt still resolves root revision2 after the root has advanced;
	// additions are independent and may use a valid older observed revision.
	third := setCommand(a, parent)
	third.Payload.ObservedWorkout = next.Payload.ObservedWorkout
	requireSetOutcome(t, u, a, third, "accepted", "")
	another := startSetWorkout(t, base, a, v)
	wrong := setCommand(a, parent)
	wrong.Payload.ObservedWorkout = protocol.Base{AfterOperationID: ptr(another.OperationID)}
	requireSetOutcome(t, u, a, wrong, "rejected", "VALIDATION_ERROR")
	failedParent := workoutCommand(a, v)
	failedParent.Base = protocol.Base{Revision: ptr("1")}
	requireWorkoutOutcome(t, workoutUseCase(t, workoutDependencies(base)), a, failedParent, "rejected", "VALIDATION_ERROR")
	blocked := setCommand(a, parent)
	blocked.Payload.ObservedWorkout = protocol.Base{AfterOperationID: ptr(failedParent.OperationID)}
	requireSetOutcome(t, u, a, blocked, "dependency_blocked", "DEPENDENCY_BLOCKED")
	_, b, baseB := trainingFixture(t)
	vb := publishedWorkoutFixture(t, baseB, b)
	foreign := startSetWorkout(t, baseB, b, vb)
	hidden := setCommand(a, parent)
	hidden.Payload.ObservedWorkout = protocol.Base{AfterOperationID: ptr(foreign.OperationID)}
	requireSetOutcome(t, u, a, hidden, "dependency_pending", "DEPENDENCY_PENDING")
	self := setCommand(a, parent)
	self.Payload.ObservedWorkout = protocol.Base{AfterOperationID: ptr(self.OperationID)}
	if _, err := u.Execute(context.Background(), a, self); !errors.Is(err, protocol.ErrCommandInvalid) {
		t.Fatal("direct cycle accepted", err)
	}
}

type failAfterSet struct {
	repository.WorkoutSetRepository
}

func (r failAfterSet) CreateAndAdvance(tx shared.Transaction, owner uuid.UUID, s *workout.Set, w *workout.Workout, expected int64) (bool, error) {
	ok, err := r.WorkoutSetRepository.CreateAndAdvance(tx, owner, s, w, expected)
	if err == nil && ok {
		return false, errors.New("synthetic post-set failure")
	}
	return ok, err
}
func TestWorkoutSetCreatePostgresCollisionsAndAtomicRollback(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, baseB := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	vb := publishedWorkoutFixture(t, baseB, b)
	w := startSetWorkout(t, base, a, v)
	wb := startSetWorkout(t, baseB, b, vb)
	d := setDependencies(base)
	u := setUseCase(t, d)
	db := setDependencies(baseB)
	own := setCommand(a, w)
	foreign := setCommand(b, wb)
	requireSetOutcome(t, u, a, own, "accepted", "")
	requireSetOutcome(t, setUseCase(t, db), b, foreign, "accepted", "")
	for _, id := range []string{own.EntityID, foreign.EntityID} {
		c := setCommand(a, w)
		c.EntityID = id
		requireSetOutcome(t, u, a, c, "rejected", "REVISION_CONFLICT")
	}
	if trainingCount(t, owner, "SELECT revision FROM mancuerna.workouts WHERE id=?", w.EntityID) != 2 {
		t.Fatal("collision advanced aggregate")
	}
	c := setCommand(a, w)
	before := trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID)
	for _, mode := range []int{0, 1} {
		broken := d
		if mode == 0 {
			broken.Sets = failAfterSet{d.Sets}
		} else {
			broken.Commands = trainingFailCommit{d.Commands}
		}
		if _, err := setUseCase(t, broken).Execute(context.Background(), a, c); err == nil {
			t.Fatal("injected error ignored")
		}
		if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workout_sets WHERE id=?", c.EntityID) != 0 || trainingCount(t, owner, "SELECT revision FROM mancuerna.workouts WHERE id=?", w.EntityID) != 2 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", c.OperationID) != 0 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != before {
			t.Fatal("set escaped full rollback")
		}
	}
	requireSetOutcome(t, u, a, c, "accepted", "")
}
func seedWorkoutState(t *testing.T, owner *gorm.DB, w workout.StartCommand, deleted bool) {
	t.Helper()
	query := "UPDATE mancuerna.workouts SET state='completed',finished_at=started_at+interval '1 hour',revision=revision+1,finalized_revision=revision+1 WHERE id=?"
	if deleted {
		query = "UPDATE mancuerna.workouts SET deleted_at=date_trunc('milliseconds',clock_timestamp()),revision=revision+1 WHERE id=?"
	}
	if err := owner.Exec(query, w.EntityID).Error; err != nil {
		t.Fatal(err)
	}
}
func TestWorkoutSetCreatePostgresFinalizedReviewAndDeletedProposal(t *testing.T) {
	owner, a, base := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	w := startSetWorkout(t, base, a, v)
	d := setDependencies(base)
	u := setUseCase(t, d)
	// State transitions are explicit SQL fixtures: complete/delete producers are
	// not implemented in this block, so this is not their end-to-end race test.
	seedWorkoutState(t, owner, w, false)
	late := setCommand(a, w)
	result := requireSetOutcome(t, u, a, late, "accepted_with_review", "")
	current, err := readWorkout(t, workoutDependencies(base), a.AccountID, uuid.MustParse(w.EntityID))
	if err != nil || current.State != "completed" || current.Revision != 3 || !current.NeedsReview || *current.FinalizedRevision != 2 || !current.FinishedAt.Equal(current.StartedAt.Add(3600000000000)) {
		t.Fatal("late result rewrote finalization", err)
	}
	var conflict struct {
		EntityType, EntityID, Reason, Proposal string
		BaseRevision, CurrentRevision          int64
	}
	if err = owner.Raw("SELECT entity_type,entity_id::text,reason,proposal::text,base_revision,current_revision FROM mancuerna.sync_conflicts WHERE id=?", result.ConflictID).Scan(&conflict).Error; err != nil {
		t.Fatal(err)
	}
	if conflict.EntityType != "workout" || conflict.EntityID != w.EntityID || conflict.BaseRevision != 1 || conflict.CurrentRevision != 3 || conflict.Reason != "finalization" || len(result.Affected) != 3 || result.Affected[2].EntityType != "conflict" {
		t.Fatal("wrong finalization conflict binding")
	}
	var proposed workout.CreateSetCommand
	if json.Unmarshal([]byte(conflict.Proposal), &proposed) != nil || !reflect.DeepEqual(proposed, late) {
		t.Fatal("series proposal not intact")
	}
	if replay := requireSetOutcome(t, u, a, late, "accepted_with_review", ""); !reflect.DeepEqual(result, replay) {
		t.Fatal("review replay changed")
	}
	if trainingCount(t, owner, "SELECT jsonb_array_length(changes) FROM mancuerna.sync_changes WHERE operation_id=?", late.OperationID) != 3 {
		t.Fatal("review effects split")
	}
	gone := startSetWorkout(t, base, a, v)
	seedWorkoutState(t, owner, gone, true)
	c := setCommand(a, gone)
	deleted := requireSetOutcome(t, u, a, c, "conflict", "ENTITY_DELETED")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workout_sets WHERE workout_id=?", gone.EntityID) != 0 || trainingCount(t, owner, "SELECT revision FROM mancuerna.workouts WHERE id=?", gone.EntityID) != 2 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.sync_conflicts WHERE id=? AND entity_type='workout' AND entity_id=? AND current_revision=2 AND reason='deleted'", deleted.ConflictID, gone.EntityID) != 1 {
		t.Fatal("deleted workout resurrected")
	}
	invalid := setCommand(a, gone)
	invalid.Payload.WorkoutExerciseID = w.Payload.Exercises[0].ID
	requireSetOutcome(t, u, a, invalid, "rejected", "VALIDATION_ERROR")
	invalid = setCommand(a, gone)
	invalid.Payload.Load = workout.LoadInput{LoadMode: "none"}
	requireSetOutcome(t, u, a, invalid, "rejected", "VALIDATION_ERROR")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.sync_conflicts WHERE entity_id=?", gone.EntityID) != 1 {
		t.Fatal("unauthorized proposal retained")
	}
}
func TestWorkoutSetCreatePostgresScopeOriginAndObservedValidation(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, baseB := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	w := startSetWorkout(t, base, a, v)
	d := setDependencies(base)
	u := setUseCase(t, d)
	foreign := setCommand(b, w)
	requireSetOutcome(t, u, b, foreign, "rejected", "NOT_FOUND")
	other := startSetWorkout(t, base, a, v)
	wrong := setCommand(a, w)
	wrong.Payload.WorkoutExerciseID = other.Payload.Exercises[0].ID
	requireSetOutcome(t, u, a, wrong, "rejected", "VALIDATION_ERROR")
	for _, observed := range []string{"0", "2", "9223372036854775807"} {
		c := setCommand(a, w)
		c.Payload.ObservedWorkout = protocol.Base{Revision: ptr(observed)}
		requireSetOutcome(t, u, a, c, "rejected", "VALIDATION_ERROR")
	}
	for _, ctx := range []context.Context{shared.WithPersonalAccount(context.Background(), b.AccountID), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := u.Execute(ctx, a, setCommand(a, w)); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed scope accepted", err)
		}
	}
	auth, err := shared.NewPersonalPostgresUnitOfWork(trainingTestPool(t, "MANCUERNA_TEST_AUTH_DSN"), shared.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	badPool := d
	badPool.UoW = auth
	if _, err = setUseCase(t, badPool).Execute(context.Background(), a, setCommand(a, w)); err == nil {
		t.Fatal("auth pool wrote set")
	}
	origin := uuid.New()
	if err = owner.Exec("INSERT INTO mancuerna.devices(id,account_id,platform,label,public_key,authorized_at,authorized_by_method,revoked_at) VALUES(?,?,'ios','Historical synthetic',?,clock_timestamp()-interval '1 day','signup',clock_timestamp())", origin, a.AccountID, make([]byte, 32)).Error; err != nil {
		t.Fatal(err)
	}
	historical := setCommand(a, w)
	historical.OriginDeviceID = origin.String()
	requireSetOutcome(t, u, a, historical, "accepted", "")
	badOrigin := setCommand(a, w)
	badOrigin.OriginDeviceID = b.DeviceID.String()
	if _, err = u.Execute(context.Background(), a, badOrigin); !errors.Is(err, protocol.ErrCommandOrigin) {
		t.Fatal("foreign origin accepted", err)
	}
	if _, err = readSet(t, setDependencies(baseB), b.AccountID, uuid.MustParse(historical.EntityID)); !errors.Is(err, trainingErrors.ErrWorkoutSetNotFound) {
		t.Fatal("B read A set", err)
	}
	if err = owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", a.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = u.Execute(context.Background(), a, historical); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("revoked actor replayed set", err)
	}
}
func TestWorkoutSetCreatePostgresLifetimeIDLimitSerializes(t *testing.T) {
	owner, a, base := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	w := startSetWorkout(t, base, a, v)
	d := setDependencies(base)
	u := setUseCase(t, d)
	// Seed capacity only; this is not evidence that 999 application operations
	// were replayed. Deleted rows deliberately count toward the lifetime limit.
	if err := owner.Exec(`INSERT INTO mancuerna.workout_sets(id,account_id,workout_id,workout_exercise_id,origin_device_id,position_hint,load_mode,weight_value,weight_unit,repetitions,captured_at,deleted_at)
 SELECT gen_random_uuid(),?::uuid,?::uuid,?::uuid,?::uuid,0,'external',30.000,'kg',0,clock_timestamp(),CASE WHEN n%2=0 THEN clock_timestamp() ELSE NULL END FROM generate_series(1,999) n`, a.AccountID, w.EntityID, w.Payload.Exercises[0].ID, a.DeviceID).Error; err != nil {
		t.Fatal(err)
	}
	commands := []workout.CreateSetCommand{setCommand(a, w), setCommand(a, w)}
	results := make([]protocol.OperationResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range commands {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = u.Execute(context.Background(), a, commands[i]) }(i)
	}
	wg.Wait()
	wins := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if r.Outcome == "accepted" {
			wins++
		} else if r.Outcome != "rejected" || r.Error.Code != "LIMIT_EXCEEDED" {
			t.Fatal("wrong limit result")
		}
	}
	if wins != 1 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workout_sets WHERE workout_id=?", w.EntityID) != 1000 || trainingCount(t, owner, "SELECT revision FROM mancuerna.workouts WHERE id=?", w.EntityID) != 2 {
		t.Fatal("lifetime ID limit exceeded")
	}
	for i, r := range results {
		if r.Outcome == "rejected" {
			requireSetOutcome(t, u, a, commands[i], "rejected", "LIMIT_EXCEEDED")
		}
	}
}

type failAfterSetConflict struct{ protocolRepository.Commands }

func (r failAfterSetConflict) SaveConflict(tx shared.Transaction, c protocol.Conflict) error {
	if err := r.Commands.SaveConflict(tx, c); err != nil {
		return err
	}
	return errors.New("synthetic conflict failure")
}
func TestWorkoutSetCreatePostgresReviewRollbackAndPreflight(t *testing.T) {
	owner, a, base := trainingFixture(t)
	v := publishedWorkoutFixture(t, base, a)
	w := startSetWorkout(t, base, a, v)
	seedWorkoutState(t, owner, w, false)
	d := setDependencies(base)
	c := setCommand(a, w)
	before := trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID)
	for _, mode := range []int{0, 1, 2} {
		broken := d
		if mode == 0 {
			broken.Sets = failAfterSet{d.Sets}
		} else if mode == 1 {
			broken.Commands = failAfterSetConflict{d.Commands}
		} else {
			broken.Commands = trainingFailCommit{d.Commands}
		}
		if _, err := setUseCase(t, broken).Execute(context.Background(), a, c); err == nil {
			t.Fatal("injected review failure ignored")
		}
		if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.workout_sets WHERE workout_id=?", w.EntityID) != 0 || trainingCount(t, owner, "SELECT revision FROM mancuerna.workouts WHERE id=?", w.EntityID) != 2 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.sync_conflicts WHERE operation_id=?", c.OperationID) != 0 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", c.OperationID) != 0 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != before {
			t.Fatal("review not atomic")
		}
	}
	limited := d
	limited.Commands = denyWorkoutChanges{d.Commands}
	limit := setCommand(a, w)
	requireSetOutcome(t, setUseCase(t, limited), a, limit, "rejected", "LIMIT_EXCEEDED")
	requireSetOutcome(t, setUseCase(t, d), a, limit, "rejected", "LIMIT_EXCEEDED")
	requireSetOutcome(t, setUseCase(t, d), a, c, "accepted_with_review", "")
	// The repository must refuse callers that attempt to change an immutable
	// field while advancing the workout or clear an existing review flag.
	current, err := readWorkout(t, workoutDependencies(base), a.AccountID, uuid.MustParse(w.EntityID))
	if err != nil {
		t.Fatal(err)
	}
	value, unit := "1.000", "kg"
	load, _ := workout.CanonicalLoad("external", &value, &unit)
	now := base.Now().UTC().Truncate(time.Millisecond)
	series, err := workout.NewSet(current, uuid.New(), a.DeviceID, current.Exercises[0].ID, 0, 1, load, now, now)
	if err != nil {
		t.Fatal(err)
	}
	next, err := current.WithNewSet(series, now)
	if err != nil {
		t.Fatal(err)
	}
	next.RoutineTitleSnapshot = "invented"
	err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), a.AccountID), func(tx shared.Transaction) error {
		_, err := d.Sets.CreateAndAdvance(tx, a.AccountID, series, next, current.Revision)
		return err
	})
	if !errors.Is(err, trainingErrors.ErrInvalidWorkoutSet) {
		t.Fatal("repository accepted arbitrary workout mutation", err)
	}
}
