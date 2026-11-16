//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func draftDependencies(base trainingApp.CreatePersonalExerciseDependencies) trainingApp.SaveRoutineDraftDependencies {
	r := routineDependencies(base)
	return trainingApp.SaveRoutineDraftDependencies{UoW: r.UoW, Routines: r.Routines, Drafts: NewRoutinePostgresRepository(), Commands: r.Commands, Stream: r.Stream, Canonicalizer: r.Canonicalizer, Now: r.Now}
}
func draftUseCase(t *testing.T, d trainingApp.SaveRoutineDraftDependencies) *trainingApp.SaveRoutineDraft {
	t.Helper()
	u, err := trainingApp.NewSaveRoutineDraft(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func draftCommand(actor security.AccessClaims, c routine.CreateCommand, revision int64) routine.SaveDraftCommand {
	return routine.SaveDraftCommand{OperationID: uuid.NewString(), OriginDeviceID: actor.DeviceID.String(), Type: routine.SaveDraftCommandType, EntityID: c.EntityID, Base: protocol.Base{Revision: ptr(strconv.FormatInt(revision, 10))}, CapturedAt: c.CapturedAt, Payload: routine.SaveDraftPayload{Content: c.Payload.Draft.Clone()}}
}
func createDraftFixture(t *testing.T, base trainingApp.CreatePersonalExerciseDependencies, actor security.AccessClaims, c routine.CreateCommand) {
	t.Helper()
	got, err := routineUseCase(t, routineDependencies(base)).Execute(context.Background(), actor, c)
	if err != nil || got.Outcome != "accepted" {
		t.Fatal("routine prerequisite failed", err)
	}
}
func requireDraftOutcome(t *testing.T, u *trainingApp.SaveRoutineDraft, a security.AccessClaims, c routine.SaveDraftCommand, outcome, code string) protocol.OperationResult {
	t.Helper()
	got, err := u.Execute(context.Background(), a, c)
	if err != nil || got.Outcome != outcome || (code != "" && (got.Error == nil || got.Error.Code != code)) {
		t.Fatal("unexpected draft outcome", got.Outcome, err)
	}
	return got
}

func TestRoutineDraftSavePostgresChainAndDraftOnlyProjection(t *testing.T) {
	owner, a, base := trainingFixture(t)
	c := routineCommand(a)
	c.Payload.Draft.Title = "Original title"
	createDraftFixture(t, base, a, c)
	d := draftDependencies(base)
	u := draftUseCase(t, d)
	save := draftCommand(a, c, 1)
	save.Base = protocol.Base{AfterOperationID: &c.OperationID}
	save.Payload.Content.Title = "  Edited Élite 漢  "
	first := requireDraftOutcome(t, u, a, save, "accepted", "")
	if len(first.Affected) != 1 || first.Affected[0].EntityType != "routine_draft" || first.Affected[0].Revision != "2" {
		t.Fatal("save changed root revision")
	}
	root, draft, err := routineRead(t, routineDependencies(base), a.AccountID, uuid.MustParse(c.EntityID))
	if err != nil {
		t.Fatal(err)
	}
	if root.Revision != 1 || root.Title != c.Payload.Draft.Title || root.Status != "draft" || root.CurrentVersionID != nil || !root.CreatedAt.Equal(root.UpdatedAt) || draft.Revision != 2 || draft.Content.Title != save.Payload.Content.Title {
		t.Fatal("save published or changed root")
	}
	if replay := requireDraftOutcome(t, u, a, save, "accepted", ""); !reflect.DeepEqual(replay, first) {
		t.Fatal("save replay changed")
	}
	var raw []byte
	if err = owner.Raw("SELECT changes FROM mancuerna.sync_changes WHERE account_id=? AND sequence=2", a.AccountID).Row().Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var changes []protocol.Change
	if json.Unmarshal(raw, &changes) != nil || len(changes) != 1 || changes[0].EntityType != "routine_draft" || changes[0].Revision != "2" {
		t.Fatal("save journal not draft-only")
	}
	var view map[string]any
	if json.Unmarshal(changes[0].Data, &view) != nil || len(view) != 5 || view["base_version_id"] != nil || view["deleted_at"] != nil {
		t.Fatal("invalid draft projection")
	}
	var stored []byte
	if err = owner.Raw("SELECT canonical FROM mancuerna.record_revisions WHERE account_id=? AND entity_type='routine_draft' AND entity_id=? AND revision=2", a.AccountID, c.EntityID).Row().Scan(&stored); err != nil {
		t.Fatal(err)
	}
	x, _ := d.Canonicalizer.Canonicalize(stored)
	y, _ := d.Canonicalizer.Canonicalize(changes[0].Data)
	if string(x) != string(y) {
		t.Fatal("save journal differs from revision")
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.record_revisions WHERE account_id=? AND entity_type='routine'", a.AccountID) != 1 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 2 {
		t.Fatal("save emitted extra effects")
	}
	changed := save
	changed.CapturedAt = "2026-10-19T00:00:00.000Z"
	if _, err = u.Execute(context.Background(), a, changed); !errors.Is(err, protocol.ErrOperationMismatch) {
		t.Fatal("captured timestamp normalized before hash", err)
	}
}

func TestRoutineDraftSavePostgresDependenciesRemainExactAndDurable(t *testing.T) {
	owner, a, base := trainingFixture(t)
	d := draftDependencies(base)
	u := draftUseCase(t, d)
	c := routineCommand(a)
	save := draftCommand(a, c, 1)
	save.Base = protocol.Base{AfterOperationID: &c.OperationID}
	requireDraftOutcome(t, u, a, save, "dependency_pending", "DEPENDENCY_PENDING")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", save.OperationID) != 0 {
		t.Fatal("pending dependency became terminal")
	}
	createDraftFixture(t, base, a, c)
	requireDraftOutcome(t, u, a, save, "accepted", "")
	old := draftCommand(a, c, 2)
	old.Base = protocol.Base{AfterOperationID: &c.OperationID}
	conflict := requireDraftOutcome(t, u, a, old, "conflict", "REVISION_CONFLICT")
	if conflict.ConflictID == "" {
		t.Fatal("old parent revision silently used current")
	}
	if trainingCount(t, owner, "SELECT base_revision FROM mancuerna.sync_conflicts WHERE id=?", conflict.ConflictID) != 1 || trainingCount(t, owner, "SELECT current_revision FROM mancuerna.sync_conflicts WHERE id=?", conflict.ConflictID) != 2 {
		t.Fatal("dependency did not preserve exact affected revision")
	}
	failed := routineCommand(a)
	failed.Base.Revision = ptr("9")
	got, err := routineUseCase(t, routineDependencies(base)).Execute(context.Background(), a, failed)
	if err != nil || got.Outcome != "rejected" {
		t.Fatal(err)
	}
	blocked := draftCommand(a, c, 2)
	blocked.Base = protocol.Base{AfterOperationID: &failed.OperationID}
	first := requireDraftOutcome(t, u, a, blocked, "dependency_blocked", "DEPENDENCY_BLOCKED")
	if replay := requireDraftOutcome(t, u, a, blocked, "dependency_blocked", "DEPENDENCY_BLOCKED"); !reflect.DeepEqual(first, replay) {
		t.Fatal("blocked receipt changed")
	}
	other := routineCommand(a)
	createDraftFixture(t, base, a, other)
	wrong := draftCommand(a, c, 2)
	wrong.Base = protocol.Base{AfterOperationID: &other.OperationID}
	requireDraftOutcome(t, u, a, wrong, "rejected", "VALIDATION_ERROR")
	exercise := trainingCommand(a)
	if _, err = trainingUseCase(t, base).Execute(context.Background(), a, exercise); err != nil {
		t.Fatal(err)
	}
	wrong.OperationID = uuid.NewString()
	wrong.Base = protocol.Base{AfterOperationID: &exercise.OperationID}
	requireDraftOutcome(t, u, a, wrong, "rejected", "VALIDATION_ERROR")
	_, b, _ := trainingFixture(t)
	foreign := routineCommand(b)
	createDraftFixture(t, base, b, foreign)
	wrong.OperationID = uuid.NewString()
	wrong.Base = protocol.Base{AfterOperationID: &foreign.OperationID}
	requireDraftOutcome(t, u, a, wrong, "dependency_pending", "DEPENDENCY_PENDING")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", wrong.OperationID) != 0 {
		t.Fatal("foreign parent leaked existence")
	}
	cycleA, cycleB := draftCommand(a, c, 2), draftCommand(a, c, 2)
	cycleA.Base = protocol.Base{AfterOperationID: &cycleB.OperationID}
	cycleB.Base = protocol.Base{AfterOperationID: &cycleA.OperationID}
	requireDraftOutcome(t, u, a, cycleA, "dependency_pending", "DEPENDENCY_PENDING")
	requireDraftOutcome(t, u, a, cycleB, "dependency_pending", "DEPENDENCY_PENDING")
	self := draftCommand(a, c, 2)
	self.Base = protocol.Base{AfterOperationID: &self.OperationID}
	if _, err = u.Execute(context.Background(), a, self); !errors.Is(err, protocol.ErrCommandInvalid) {
		t.Fatal("self dependency accepted", err)
	}
}

func TestRoutineDraftSavePostgresConcurrentCASAndConflictPreservation(t *testing.T) {
	owner, a, base := trainingFixture(t)
	c := routineCommand(a)
	createDraftFixture(t, base, a, c)
	d := draftDependencies(base)
	u := draftUseCase(t, d)
	commands := []routine.SaveDraftCommand{draftCommand(a, c, 1), draftCommand(a, c, 1)}
	commands[0].Payload.Content.Title = "Proposal A"
	commands[1].Payload.Content.Title = "Proposal B"
	results := make([]protocol.OperationResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range commands {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = u.Execute(context.Background(), a, commands[i])
		}(i)
	}
	close(start)
	wg.Wait()
	accepted, conflicted := -1, -1
	for i, r := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if r.Outcome == "accepted" {
			if accepted != -1 {
				t.Fatal("both saves accepted")
			}
			accepted = i
		} else if r.Outcome == "conflict" {
			conflicted = i
		} else {
			t.Fatal("wrong CAS outcome", r.Outcome)
		}
	}
	if accepted < 0 || conflicted < 0 {
		t.Fatal("CAS did not preserve competing change")
	}
	_, draft, err := routineRead(t, routineDependencies(base), a.AccountID, uuid.MustParse(c.EntityID))
	if err != nil || draft.Revision != 2 || draft.Content.Title != commands[accepted].Payload.Content.Title {
		t.Fatal("losing proposal overwrote draft", err)
	}
	var proposal []byte
	if err = owner.Raw("SELECT proposal FROM mancuerna.sync_conflicts WHERE account_id=? AND id=?", a.AccountID, results[conflicted].ConflictID).Row().Scan(&proposal); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(commands[conflicted])
	x, _ := d.Canonicalizer.Canonicalize(raw)
	y, _ := d.Canonicalizer.Canonicalize(proposal)
	if string(x) != string(y) {
		t.Fatal("conflict lost original command")
	}
	for i := range commands {
		if got := requireDraftOutcome(t, u, a, commands[i], results[i].Outcome, ""); !reflect.DeepEqual(got, results[i]) {
			t.Fatal("concurrent result replay changed")
		}
	}
	if trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 3 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.sync_conflicts WHERE account_id=?", a.AccountID) != 1 {
		t.Fatal("CAS replay duplicated effects")
	}
	// Same operation under concurrent retries must produce only one mutation.
	next := draftCommand(a, c, 2)
	const attempts = 5
	rs := make([]protocol.OperationResult, attempts)
	es := make([]error, attempts)
	for i := range rs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); rs[i], es[i] = u.Execute(context.Background(), a, next) }(i)
	}
	wg.Wait()
	for i := range rs {
		if es[i] != nil || rs[i].Outcome != "accepted" || !reflect.DeepEqual(rs[i], rs[0]) {
			t.Fatal("concurrent replay diverged", es[i])
		}
	}
	if trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 4 {
		t.Fatal("same save repeated")
	}
}

type failAfterDraftSave struct {
	repository.RoutineDraftRepository
}

func (r failAfterDraftSave) Save(tx shared.Transaction, owner uuid.UUID, next *routine.DraftRecord, revision int64) (bool, error) {
	ok, err := r.RoutineDraftRepository.Save(tx, owner, next, revision)
	if err != nil {
		return false, err
	}
	if ok {
		return false, errors.New("synthetic post-save failure")
	}
	return false, nil
}

func TestRoutineDraftSavePostgresRollbackAndReopenKeepRootUnchanged(t *testing.T) {
	owner, a, base := trainingFixture(t)
	c := routineCommand(a)
	createDraftFixture(t, base, a, c)
	d := draftDependencies(base)
	save := draftCommand(a, c, 1)
	save.Payload.Content.Title = "Replacement"
	for _, failure := range []int{0, 1} {
		broken := d
		if failure == 0 {
			broken.Drafts = failAfterDraftSave{d.Drafts}
		} else {
			broken.Commands = trainingFailCommit{d.Commands}
		}
		if _, err := draftUseCase(t, broken).Execute(context.Background(), a, save); err == nil {
			t.Fatal("injected failure ignored")
		}
		root, draft, err := routineRead(t, routineDependencies(base), a.AccountID, uuid.MustParse(c.EntityID))
		if err != nil || root.Revision != 1 || draft.Revision != 1 || draft.Content.Title != "" {
			t.Fatal("rollback left changed draft", err)
		}
		if trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 1 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", save.OperationID) != 0 {
			t.Fatal("rollback left receipt or sequence")
		}
	}
	u := draftUseCase(t, d)
	requireDraftOutcome(t, u, a, save, "accepted", "")
	// This prerequisite simulates a discarded draft, not a delivered discard command.
	if err := owner.Exec("UPDATE mancuerna.routine_drafts SET content=NULL,deleted_at=date_trunc('milliseconds',clock_timestamp()),updated_at=date_trunc('milliseconds',clock_timestamp()),revision=3 WHERE account_id=? AND routine_id=?", a.AccountID, c.EntityID).Error; err != nil {
		t.Fatal(err)
	}
	reopen := draftCommand(a, c, 3)
	requireDraftOutcome(t, u, a, reopen, "accepted", "")
	root, draft, err := routineRead(t, routineDependencies(base), a.AccountID, uuid.MustParse(c.EntityID))
	if err != nil || draft.Revision != 4 || draft.DeletedAt != nil || draft.Content == nil || root.Revision != 1 || root.Status != "draft" {
		t.Fatal("reopen reset or published", err)
	}
	if err = owner.Exec("UPDATE mancuerna.routines SET status='archived' WHERE account_id=? AND id=?", a.AccountID, c.EntityID).Error; err != nil {
		t.Fatal(err)
	}
	archived := draftCommand(a, c, 4)
	requireDraftOutcome(t, u, a, archived, "rejected", "REVISION_CONFLICT")
	if err = owner.Exec("UPDATE mancuerna.routines SET deleted_at=clock_timestamp() WHERE account_id=? AND id=?", a.AccountID, c.EntityID).Error; err != nil {
		t.Fatal(err)
	}
	deleted := draftCommand(a, c, 4)
	result := requireDraftOutcome(t, u, a, deleted, "conflict", "ENTITY_DELETED")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.sync_conflicts WHERE id=? AND reason='deleted'", result.ConflictID) != 1 || trainingCount(t, owner, "SELECT revision FROM mancuerna.routine_drafts WHERE routine_id=?", c.EntityID) != 4 {
		t.Fatal("deleted root resurrected or proposal lost")
	}
}

func TestRoutineDraftSavePostgresHistoricalReferencesRequireExactAppearance(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	personal := trainingCommand(a)
	foreign := trainingCommand(b)
	for _, p := range []struct {
		actor     security.AccessClaims
		commandID int
	}{{a, 0}, {b, 1}} {
		cmd := personal
		if p.commandID == 1 {
			cmd = foreign
		}
		if _, err := trainingUseCase(t, base).Execute(context.Background(), p.actor, cmd); err != nil {
			t.Fatal(err)
		}
	}
	c := withRoutineReference(routineCommand(a), &routine.ExerciseRef{Source: "personal", ID: personal.EntityID})
	createDraftFixture(t, base, a, c)
	d := draftDependencies(base)
	u := draftUseCase(t, d)
	if err := owner.Exec("UPDATE mancuerna.personal_exercises SET archived_at=clock_timestamp(),deleted_at=clock_timestamp() WHERE id=?", personal.EntityID).Error; err != nil {
		t.Fatal(err)
	}
	same := draftCommand(a, c, 1)
	same.Payload.Content.Days[0].Exercises[0].Notes = "Historical note"
	requireDraftOutcome(t, u, a, same, "accepted", "")
	for _, change := range []func(*routine.SaveDraftCommand){
		func(s *routine.SaveDraftCommand) { s.Payload.Content.Days[0].Exercises[0].ID = uuid.NewString() },
		func(s *routine.SaveDraftCommand) {
			s.Payload.Content.Days[0].Exercises[0].ExerciseRef.ID = foreign.EntityID
		},
		func(s *routine.SaveDraftCommand) {
			s.Payload.Content.Days[0].Exercises[0].ExerciseRef.ID = uuid.NewString()
		},
	} {
		bad := draftCommand(a, c, 2)
		change(&bad)
		requireDraftOutcome(t, u, a, bad, "rejected", "VALIDATION_ERROR")
	}
	// Version fixture supplies pre-existing immutable history; publishing is not implemented by this block.
	version, appearance, day := seedRoutineVersion(t, owner, a, uuid.MustParse(c.EntityID), uuid.MustParse(personal.EntityID), 1)
	empty := draftCommand(a, c, 2)
	empty.Payload.Content.Days = []routine.DraftDay{}
	requireDraftOutcome(t, u, a, empty, "accepted", "")
	fromHistory := draftCommand(a, c, 3)
	fromHistory.Payload.BaseVersionID = ptr(version.String())
	fromHistory.Payload.Content.Days[0].ID = day.String()
	fromHistory.Payload.Content.Days[0].Exercises[0].ID = appearance.String()
	requireDraftOutcome(t, u, a, fromHistory, "accepted", "")
	newOccurrence := fromHistory
	newOccurrence.OperationID = uuid.NewString()
	newOccurrence.Base = protocol.Base{Revision: ptr("4")}
	newOccurrence.Payload.Content = fromHistory.Payload.Content.Clone()
	newOccurrence.Payload.Content.Days[0].Exercises[0].ID = uuid.NewString()
	requireDraftOutcome(t, u, a, newOccurrence, "rejected", "VALIDATION_ERROR")
	other := routineCommand(a)
	createDraftFixture(t, base, a, other)
	wrongVersion := draftCommand(a, other, 1)
	wrongVersion.Payload.BaseVersionID = ptr(version.String())
	requireDraftOutcome(t, u, a, wrongVersion, "rejected", "VALIDATION_ERROR")
	foreignRoutine := routineCommand(b)
	createDraftFixture(t, base, b, foreignRoutine)
	foreignVersion, _, _ := seedRoutineVersion(t, owner, b, uuid.MustParse(foreignRoutine.EntityID), uuid.MustParse(foreign.EntityID), 1)
	for _, id := range []uuid.UUID{foreignVersion, uuid.New()} {
		bad := draftCommand(a, c, 4)
		bad.Payload.BaseVersionID = ptr(id.String())
		requireDraftOutcome(t, u, a, bad, "rejected", "VALIDATION_ERROR")
	}
	// An earlier sealed version is legitimate provenance even when a newer one is current.
	latest, _, _ := seedRoutineVersion(t, owner, a, uuid.MustParse(c.EntityID), uuid.MustParse(personal.EntityID), 2)
	if err := owner.Exec("UPDATE mancuerna.routines SET status='ready',current_version_id=? WHERE account_id=? AND id=?", latest, a.AccountID, c.EntityID).Error; err != nil {
		t.Fatal(err)
	}
	oldBase := fromHistory
	oldBase.OperationID = uuid.NewString()
	oldBase.Base = protocol.Base{Revision: ptr("4")}
	requireDraftOutcome(t, u, a, oldBase, "accepted", "")
	root, draft, err := routineRead(t, routineDependencies(base), a.AccountID, uuid.MustParse(c.EntityID))
	if err != nil || root.CurrentVersionID == nil || *root.CurrentVersionID != latest || root.Revision != 1 || draft.BaseVersionID == nil || *draft.BaseVersionID != version {
		t.Fatal("historical save changed current version", err)
	}
}

func seedRoutineVersion(t *testing.T, owner *gorm.DB, a security.AccessClaims, routineID, exerciseID uuid.UUID, number int) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	version, day, appearance := uuid.New(), uuid.New(), uuid.New()
	hash := sha256.Sum256([]byte("synthetic existing immutable version"))
	err := owner.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("INSERT INTO mancuerna.routine_versions(id,account_id,routine_id,version_number,title_snapshot,content_hash,source_draft_revision,actor_device_id) VALUES(?,?,?,?,'Existing version',?,1,?)", version, a.AccountID, routineID, number, hash[:], a.DeviceID).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO mancuerna.routine_days(id,account_id,version_id,label,position) VALUES(?,?,?,'Day',0)", day, a.AccountID, version).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO mancuerna.routine_exercises(id,account_id,version_id,day_id,personal_exercise_id,name_snapshot,load_mode_snapshot,position,prescribed_sets,prescribed_reps) VALUES(?,?,?,?,?,'Existing exercise','external',0,3,'8')", appearance, a.AccountID, version, day, exerciseID).Error; err != nil {
			return err
		}
		return tx.Exec("UPDATE mancuerna.routine_versions SET sealed=true WHERE id=?", version).Error
	})
	if err != nil {
		t.Fatal("existing version prerequisite failed", err)
	}
	return version, appearance, day
}

func TestRoutineDraftSavePostgresAuthorityScopeAndOwnerAreRequired(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	c := routineCommand(a)
	createDraftFixture(t, base, a, c)
	d := draftDependencies(base)
	u := draftUseCase(t, d)
	foreign := draftCommand(b, c, 1)
	requireDraftOutcome(t, u, b, foreign, "rejected", "NOT_FOUND")
	save := draftCommand(a, c, 1)
	ctx := shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1})
	if _, err := u.Execute(ctx, a, save); err == nil {
		t.Fatal("mixed attempt scope accepted")
	}
	ctx = shared.WithTenant(context.Background(), uuid.New())
	if _, err := u.Execute(ctx, a, save); err == nil {
		t.Fatal("gym scope accepted")
	}
	authUoW, err := shared.NewPersonalPostgresUnitOfWork(trainingTestPool(t, "MANCUERNA_TEST_AUTH_DSN"), shared.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	bad := d
	bad.UoW = authUoW
	if _, err = draftUseCase(t, bad).Execute(context.Background(), a, save); err == nil {
		t.Fatal("auth pool wrote personal draft")
	}
	if err = owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", a.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = u.Execute(context.Background(), a, save); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("revoked actor saved draft", err)
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", save.OperationID) != 0 || trainingCount(t, owner, "SELECT revision FROM mancuerna.routine_drafts WHERE routine_id=?", c.EntityID) != 1 {
		t.Fatal("authority failure caused effects")
	}
}

func TestRoutineDraftSavePostgresLimitsFailClosedWithoutPartialEffects(t *testing.T) {
	owner, a, base := trainingFixture(t)
	c := routineCommand(a)
	createDraftFixture(t, base, a, c)
	d := draftDependencies(base)
	u := draftUseCase(t, d)
	large := largeRoutineCommand(t, routineDependencies(base), a, 1048576+500)
	save := draftCommand(a, c, 1)
	save.Payload.Content = large.Payload.Draft
	if _, err := u.Execute(context.Background(), a, save); !errors.Is(err, trainingErrors.ErrTrainingLimit) {
		t.Fatal("oversize save accepted", err)
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", save.OperationID) != 0 {
		t.Fatal("oversize command entered transaction")
	}
	large = largeRoutineCommand(t, routineDependencies(base), a, 1048576-4000)
	save.OperationID = uuid.NewString()
	save.Payload.Content = large.Payload.Draft
	requireDraftOutcome(t, u, a, save, "rejected", "LIMIT_EXCEEDED")
	if trainingCount(t, owner, "SELECT revision FROM mancuerna.routine_drafts WHERE routine_id=?", c.EntityID) != 1 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 1 {
		t.Fatal("storage limit partially changed draft")
	}
	contentFits := routineWithJSONBContentSize(t, owner, routineDependencies(base), a, 1048576-16)
	save.OperationID = uuid.NewString()
	save.Payload.Content = contentFits.Payload.Draft
	first := requireDraftOutcome(t, u, a, save, "rejected", "LIMIT_EXCEEDED")
	if replay := requireDraftOutcome(t, u, a, save, "rejected", "LIMIT_EXCEEDED"); !reflect.DeepEqual(first, replay) {
		t.Fatal("journal-only storage limit not durable")
	}
	if trainingCount(t, owner, "SELECT revision FROM mancuerna.routine_drafts WHERE routine_id=?", c.EntityID) != 1 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 1 {
		t.Fatal("journal-only limit escaped preflight")
	}
	// A stale large proposal must fit its entire conflict group before any conflict is stored.
	valid := draftCommand(a, c, 1)
	requireDraftOutcome(t, u, a, valid, "accepted", "")
	save.OperationID = uuid.NewString()
	requireDraftOutcome(t, u, a, save, "rejected", "LIMIT_EXCEEDED")
	large = largeRoutineCommand(t, routineDependencies(base), a, 1048576-200)
	save.OperationID = uuid.NewString()
	save.Payload.Content = large.Payload.Draft
	requireDraftOutcome(t, u, a, save, "rejected", "LIMIT_EXCEEDED")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.sync_conflicts WHERE account_id=?", a.AccountID) != 0 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != 2 {
		t.Fatal("oversize conflict partially committed")
	}
}
