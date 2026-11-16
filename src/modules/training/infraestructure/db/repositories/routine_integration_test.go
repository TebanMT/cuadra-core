//go:build server && integration

package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	commandRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func routineDependencies(base trainingApp.CreatePersonalExerciseDependencies) trainingApp.CreateRoutineDependencies {
	return trainingApp.CreateRoutineDependencies{UoW: base.UoW, Routines: NewRoutinePostgresRepository(), Commands: base.Commands, Stream: commandRepos.NewStream(), Canonicalizer: base.Canonicalizer, Now: base.Now}
}
func routineUseCase(t *testing.T, d trainingApp.CreateRoutineDependencies) *trainingApp.CreateRoutine {
	t.Helper()
	u, err := trainingApp.NewCreateRoutine(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func routineCommand(actor security.AccessClaims) routine.CreateCommand {
	return routine.CreateCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), Type: routine.CreateCommandType, EntityID: uuid.NewString(), Base: protocol.Base{Revision: ptr("0")}, CapturedAt: "2026-10-19T00:00:00Z", Payload: routine.CreatePayload{Draft: routine.Draft{FormatVersion: 1, Title: "", Days: []routine.DraftDay{}}}}
}
func withRoutineReference(c routine.CreateCommand, ref *routine.ExerciseRef) routine.CreateCommand {
	c.Payload.Draft.Days = []routine.DraftDay{{ID: uuid.NewString(), Label: "", Exercises: []routine.DraftExercise{{ID: uuid.NewString(), ExerciseRef: ref, PrescribedSets: nil, PrescribedReps: "", RestSeconds: nil, Notes: ""}}}}
	return c
}
func routineRead(t *testing.T, d trainingApp.CreateRoutineDependencies, ownerID, id uuid.UUID) (*routine.Routine, *routine.DraftRecord, error) {
	t.Helper()
	tx, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), ownerID))
	if err != nil {
		return nil, nil, err
	}
	defer shared.CloseQuery(tx)
	r, err := d.Routines.GetByID(tx, ownerID, id)
	if err != nil {
		return nil, nil, err
	}
	draft, err := d.Routines.GetDraftByID(tx, ownerID, id)
	return r, draft, err
}
func assertNoRoutine(t *testing.T, owner *gorm.DB, accountID uuid.UUID) {
	t.Helper()
	for _, table := range []string{"routines", "routine_drafts", "routine_versions"} {
		if trainingCount(t, owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", accountID) != 0 {
			t.Fatal("routine effect escaped failure", table)
		}
	}
}

func TestRoutineCreatePostgresConcurrentIncompleteDraftAndCompleteGroup(t *testing.T) {
	owner, a, base := trainingFixture(t)
	d := routineDependencies(base)
	u := routineUseCase(t, d)
	c := withRoutineReference(routineCommand(a), nil)
	c.Payload.Draft.Title = "  Fuerza Élite 漢  "
	const count = 6
	results, errs := make([]protocol.OperationResult, count), make([]error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; results[i], errs[i] = u.Execute(context.Background(), a, c) }(i)
	}
	close(start)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Outcome != "accepted" || !reflect.DeepEqual(results[i], results[0]) {
			t.Fatal("concurrent draft replay diverged", errs[i])
		}
	}
	if len(results[0].Affected) != 2 || results[0].Affected[0].EntityType != "routine" || results[0].Affected[1].EntityType != "routine_draft" || results[0].Affected[0].Revision != "1" || results[0].Affected[1].Revision != "1" {
		t.Fatal("creation omitted an affected aggregate")
	}
	r, draft, err := routineRead(t, d, a.AccountID, uuid.MustParse(c.EntityID))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "draft" || r.Title != c.Payload.Draft.Title || r.CurrentVersionID != nil || r.DuplicatedFromVersionID != nil || draft.BaseVersionID != nil || draft.Content == nil || !reflect.DeepEqual(*draft.Content, c.Payload.Draft) {
		t.Fatal("incomplete draft changed or published")
	}
	for table, count := range map[string]int64{"routines": 1, "routine_drafts": 1, "routine_versions": 0, "operation_receipts": 1, "record_revisions": 2, "sync_changes": 1} {
		if trainingCount(t, owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", a.AccountID) != count {
			t.Fatal("wrong atomic effects", table)
		}
	}
	if trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 1 {
		t.Fatal("two sequences used for one command")
	}
	var raw []byte
	if err = owner.Raw("SELECT changes FROM mancuerna.sync_changes WHERE account_id=?", a.AccountID).Row().Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var group []protocol.Change
	if json.Unmarshal(raw, &group) != nil || len(group) != 2 {
		t.Fatal("journal split creation")
	}
	for _, change := range group {
		var stored []byte
		if err = owner.Raw("SELECT canonical FROM mancuerna.record_revisions WHERE account_id=? AND entity_type=? AND entity_id=? AND revision=1", a.AccountID, change.EntityType, c.EntityID).Row().Scan(&stored); err != nil {
			t.Fatal(err)
		}
		x, _ := d.Canonicalizer.Canonicalize(stored)
		y, _ := d.Canonicalizer.Canonicalize(change.Data)
		if string(x) != string(y) {
			t.Fatal("revision and journal differ")
		}
		var view map[string]any
		if json.Unmarshal(stored, &view) != nil {
			t.Fatal("invalid canonical read shape")
		}
		if view["id"] != c.EntityID || view["revision"] != "1" || view["deleted_at"] != nil {
			t.Fatal("wrong canonical identity")
		}
		if change.EntityType == "routine" && (len(view) != 9 || view["status"] != "draft" || view["current_version_id"] != nil || view["created_at"] != results[0].CommittedAt) {
			t.Fatal("root projection not closed")
		}
		if change.EntityType == "routine_draft" && (len(view) != 5 || view["base_version_id"] != nil || view["content"] == nil) {
			t.Fatal("draft projection not closed")
		}
	}
	empty := routineCommand(a)
	if got, err := u.Execute(context.Background(), a, empty); err != nil || got.Outcome != "accepted" {
		t.Fatal("empty draft was treated as publication", err)
	}
}

func TestRoutineCreatePostgresReferencesOwnerAndReplayBeforeAvailability(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	d := routineDependencies(base)
	u := routineUseCase(t, d)
	personal := trainingCommand(a)
	if _, err := trainingUseCase(t, base).Execute(context.Background(), a, personal); err != nil {
		t.Fatal(err)
	}
	foreign := trainingCommand(b)
	if _, err := trainingUseCase(t, base).Execute(context.Background(), b, foreign); err != nil {
		t.Fatal(err)
	}
	c := withRoutineReference(routineCommand(a), &routine.ExerciseRef{Source: "personal", ID: personal.EntityID})
	first, err := u.Execute(context.Background(), a, c)
	if err != nil || first.Outcome != "accepted" {
		t.Fatal("real own exercise reference rejected", err)
	}
	if err = owner.Exec("UPDATE mancuerna.personal_exercises SET archived_at=clock_timestamp() WHERE id=?", personal.EntityID).Error; err != nil {
		t.Fatal(err)
	}
	if replay, err := u.Execute(context.Background(), a, c); err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatal("receipt replay incorrectly revalidated old reference", err)
	}
	for _, id := range []string{personal.EntityID, foreign.EntityID, uuid.NewString()} {
		invalid := withRoutineReference(routineCommand(a), &routine.ExerciseRef{Source: "personal", ID: id})
		got, err := u.Execute(context.Background(), a, invalid)
		if err != nil || got.Outcome != "rejected" || got.Error.Code != "VALIDATION_ERROR" {
			t.Fatal("retired/foreign/missing reference accepted", err)
		}
		if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.routines WHERE id=?", invalid.EntityID) != 0 {
			t.Fatal("invalid reference left routine")
		}
	}
	if err = owner.Exec("UPDATE mancuerna.personal_exercises SET archived_at=NULL,deleted_at=clock_timestamp() WHERE id=?", personal.EntityID).Error; err != nil {
		t.Fatal(err)
	}
	deleted := withRoutineReference(routineCommand(a), &routine.ExerciseRef{Source: "personal", ID: personal.EntityID})
	if got, err := u.Execute(context.Background(), a, deleted); err != nil || got.Outcome != "rejected" {
		t.Fatal("deleted new reference accepted", err)
	}
	catalog := uuid.New()
	if err = owner.Exec("INSERT INTO mancuerna.catalog_exercises(id,name,search_key,load_mode) VALUES(?,'Synthetic catalog','synthetic catalog','none')", catalog).Error; err != nil {
		t.Fatal(err)
	}
	global := withRoutineReference(routineCommand(a), &routine.ExerciseRef{Source: "catalog", ID: catalog.String()})
	if got, err := u.Execute(context.Background(), a, global); err != nil || got.Outcome != "accepted" {
		t.Fatal("existing catalog reference rejected", err)
	}
	if err = owner.Exec("UPDATE mancuerna.catalog_exercises SET archived_at=clock_timestamp() WHERE id=?", catalog).Error; err != nil {
		t.Fatal(err)
	}
	global.OperationID = uuid.NewString()
	global.EntityID = uuid.NewString()
	if got, err := u.Execute(context.Background(), a, global); err != nil || got.Outcome != "rejected" {
		t.Fatal("archived catalog used for new draft", err)
	}
	if _, _, err := routineRead(t, d, b.AccountID, uuid.MustParse(c.EntityID)); !errors.Is(err, trainingErrors.ErrRoutineNotFound) {
		t.Fatal("B read A draft", err)
	}
}

func TestRoutineCreatePostgresCollisionAndRollbackAcrossBothAggregates(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	d := routineDependencies(base)
	c := routineCommand(a)
	failing := d
	failing.Commands = trainingFailCommit{d.Commands}
	if _, err := routineUseCase(t, failing).Execute(context.Background(), a, c); err == nil {
		t.Fatal("post-journal failure ignored")
	}
	assertNoRoutine(t, owner, a.AccountID)
	trainingAssertEmpty(t, owner, a.AccountID)
	failing = d
	failing.Routines = routineFailAfterCreate{d.Routines}
	if _, err := routineUseCase(t, failing).Execute(context.Background(), a, c); err == nil {
		t.Fatal("post-draft failure ignored")
	}
	assertNoRoutine(t, owner, a.AccountID)
	trainingAssertEmpty(t, owner, a.AccountID)
	u := routineUseCase(t, d)
	if got, err := u.Execute(context.Background(), a, c); err != nil || got.Outcome != "accepted" {
		t.Fatal("rollback consumed identity", err)
	}
	for _, actor := range []security.AccessClaims{a, b} {
		collision := routineCommand(actor)
		collision.EntityID = c.EntityID
		got, err := u.Execute(context.Background(), actor, collision)
		if err != nil || got.Outcome != "rejected" || got.Error.Code != "REVISION_CONFLICT" {
			t.Fatal("root collision overwrote or leaked", err)
		}
		if replay, err := u.Execute(context.Background(), actor, collision); err != nil || !reflect.DeepEqual(got, replay) {
			t.Fatal("collision receipt not stable", err)
		}
	}
	foreign := routineCommand(b)
	foreign.OperationID = c.OperationID
	if _, err := u.Execute(context.Background(), b, foreign); !errors.Is(err, protocol.ErrOperationMismatch) {
		t.Fatal("global operation collision accepted", err)
	}
	assertNoRoutine(t, owner, b.AccountID)
	changed := c
	changed.Payload.Draft.Title = "Changed"
	if _, err := u.Execute(context.Background(), a, changed); !errors.Is(err, protocol.ErrOperationMismatch) {
		t.Fatal("draft changed under existing operation", err)
	}
}

type routineFailAfterCreate struct{ repository.RoutineRepository }

func (r routineFailAfterCreate) Create(tx shared.Transaction, owner uuid.UUID, root *routine.Routine, draft *routine.DraftRecord) (bool, error) {
	created, err := r.RoutineRepository.Create(tx, owner, root, draft)
	if err != nil {
		return false, err
	}
	if !created {
		return false, nil
	}
	return false, errors.New("synthetic failure after draft insertion")
}

func TestRoutineCreatePostgresRejectsWithdrawnActorMixedScopeAndInvalidBase(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	d := routineDependencies(base)
	u := routineUseCase(t, d)
	c := routineCommand(a)
	for _, ctx := range []context.Context{shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithPersonalAccount(context.Background(), b.AccountID), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := u.Execute(ctx, a, c); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed scope accepted", err)
		}
	}
	foreign := c
	foreign.OriginDeviceID = b.DeviceID.String()
	if _, err := u.Execute(context.Background(), a, foreign); !errors.Is(err, protocol.ErrCommandOrigin) {
		t.Fatal("foreign origin accepted", err)
	}
	bad := a
	bad.DeviceID = b.DeviceID
	if _, err := u.Execute(context.Background(), bad, c); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("foreign actor device accepted", err)
	}
	for _, revision := range []protocol.Base{{Revision: ptr("1")}, {AfterOperationID: ptr(uuid.NewString())}} {
		invalid := routineCommand(a)
		invalid.Base = revision
		if got, err := u.Execute(context.Background(), a, invalid); err != nil || got.Outcome != "rejected" {
			t.Fatal("noninitial creation accepted", err)
		}
	}
	assertNoRoutine(t, owner, a.AccountID)
	if got, err := u.Execute(context.Background(), a, c); err != nil || got.Outcome != "accepted" {
		t.Fatal(err)
	}
	if err := owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", a.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(context.Background(), a, c); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("withdrawn actor replayed receipt", err)
	}
}

func largeRoutineCommand(t *testing.T, d trainingApp.CreateRoutineDependencies, a security.AccessClaims, target int) routine.CreateCommand {
	t.Helper()
	c := routineCommand(a)
	c.Payload.Draft.Title = strings.Repeat("t", 120)
	for day := 0; day < 14; day++ {
		next := routine.DraftDay{ID: uuid.NewString(), Label: "", Exercises: []routine.DraftExercise{}}
		for i := 0; i < 50; i++ {
			next.Exercises = append(next.Exercises, routine.DraftExercise{ID: uuid.NewString(), PrescribedReps: "", Notes: ""})
		}
		c.Payload.Draft.Days = append(c.Payload.Draft.Days, next)
	}
	raw, _ := json.Marshal(c)
	canonical, err := d.Canonicalizer.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	remaining := target - len(canonical)
	for i := range c.Payload.Draft.Days {
		for j := range c.Payload.Draft.Days[i].Exercises {
			n := remaining
			if n > 4000 {
				n = 4000
			}
			if n < 0 {
				t.Fatal("requested fixture too small")
			}
			text := strings.Repeat("😀", n/4)
			switch n % 4 {
			case 1:
				text += "x"
			case 2:
				text += "é"
			case 3:
				text += "漢"
			}
			c.Payload.Draft.Days[i].Exercises[j].Notes = text
			remaining -= n
		}
	}
	if remaining != 0 || c.ValidateEnvelope() != nil {
		t.Fatal("large fixture violates typed bounds")
	}
	raw, _ = json.Marshal(c)
	canonical, err = d.Canonicalizer.Canonicalize(raw)
	if err != nil || len(canonical) != target {
		t.Fatal("large fixture did not reach exact canonical size", err)
	}
	return c
}

type countRoutineCreate struct {
	repository.RoutineRepository
	calls int
}

func (r *countRoutineCreate) Create(tx shared.Transaction, owner uuid.UUID, root *routine.Routine, draft *routine.DraftRecord) (bool, error) {
	r.calls++
	return r.RoutineRepository.Create(tx, owner, root, draft)
}

// The SQL representation adds spaces independently of the canonical transport
// size. Build this fixture by measuring real PostgreSQL rather than assuming
// an implementation-specific per-field byte overhead.
func routineWithJSONBContentSize(t *testing.T, owner *gorm.DB, d trainingApp.CreateRoutineDependencies, a security.AccessClaims, target int) routine.CreateCommand {
	t.Helper()
	const initial = 1048576 - 50000
	c := largeRoutineCommand(t, d, a, initial)
	measure := func(c routine.CreateCommand) int {
		t.Helper()
		raw, err := json.Marshal(c.Payload.Draft)
		if err != nil {
			t.Fatal(err)
		}
		var size int
		if err = owner.Raw("SELECT octet_length((?::jsonb)::text)", string(raw)).Row().Scan(&size); err != nil {
			t.Fatal(err)
		}
		return size
	}
	c = largeRoutineCommand(t, d, a, initial+target-measure(c))
	if measure(c) != target {
		t.Fatal("JSONB fixture did not reach exact content size")
	}
	raw, _ := json.Marshal(c)
	canonical, err := d.Canonicalizer.Canonicalize(raw)
	if err != nil || len(canonical) >= 1048576 {
		t.Fatal("content fixture already exceeds transport bound", err)
	}
	return c
}

func TestRoutineCreatePostgresCanonicalAndConservativeStorageLimits(t *testing.T) {
	owner, a, base := trainingFixture(t)
	d := routineDependencies(base)
	spy := &countRoutineCreate{RoutineRepository: d.Routines}
	d.Routines = spy
	u := routineUseCase(t, d)
	tooLarge := largeRoutineCommand(t, d, a, 1048577)
	if _, err := u.Execute(context.Background(), a, tooLarge); !errors.Is(err, trainingErrors.ErrTrainingLimit) {
		t.Fatal("canonical input bound ignored", err)
	}
	if spy.calls != 0 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE account_id=?", a.AccountID) != 0 {
		t.Fatal("oversized input reached effect")
	}
	groupTooLarge := largeRoutineCommand(t, d, a, 1048575)
	got, err := u.Execute(context.Background(), a, groupTooLarge)
	if err != nil || got.Outcome != "rejected" || got.Error.Code != "LIMIT_EXCEEDED" || spy.calls != 0 {
		t.Fatal("full group bound did not reject before inserts", err)
	}
	storageTooLarge := largeRoutineCommand(t, d, a, 1048576-4000)
	content, _ := json.Marshal(storageTooLarge.Payload.Draft)
	var dbBytes int
	if err = owner.Raw("SELECT octet_length((?::jsonb)::text)", string(content)).Row().Scan(&dbBytes); err != nil || dbBytes <= 1048576 {
		t.Fatal("fixture did not exercise conservative JSONB difference", err)
	}
	got, err = u.Execute(context.Background(), a, storageTooLarge)
	if err != nil || got.Outcome != "rejected" || got.Error.Code != "LIMIT_EXCEEDED" || spy.calls != 0 {
		t.Fatal("conservative storage bound did not fail closed", err)
	}
	// Content itself fits 059; only the additional journal envelope makes its
	// JSONB representation too large. It must be a terminal size result, not 503.
	journalTooLarge := routineWithJSONBContentSize(t, owner, d, a, 1048576-16)
	got, err = u.Execute(context.Background(), a, journalTooLarge)
	if err != nil || got.Outcome != "rejected" || got.Error.Code != "LIMIT_EXCEEDED" || spy.calls != 0 {
		t.Fatal("journal JSONB bound failed after business effects", err)
	}
	if replay, replayErr := u.Execute(context.Background(), a, journalTooLarge); replayErr != nil || !reflect.DeepEqual(got, replay) {
		t.Fatal("journal size rejection not durable", replayErr)
	}
	assertNoRoutine(t, owner, a.AccountID)
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE account_id=?", a.AccountID) != 3 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 0 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.record_revisions WHERE account_id=?", a.AccountID) != 0 {
		t.Fatal("limit rejection produced partial data/journal")
	}
}
