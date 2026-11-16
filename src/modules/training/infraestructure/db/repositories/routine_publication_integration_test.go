//go:build server && integration

package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
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
	"github.com/google/uuid"
)

func publicationDependencies(base trainingApp.CreatePersonalExerciseDependencies) trainingApp.PublishRoutineDependencies {
	return trainingApp.PublishRoutineDependencies{UoW: base.UoW, Routines: NewRoutinePostgresRepository(), Publications: NewRoutinePublicationPostgresRepository(), Commands: base.Commands, Canonicalizer: base.Canonicalizer, Now: base.Now}
}
func publicationUseCase(t *testing.T, d trainingApp.PublishRoutineDependencies) *trainingApp.PublishRoutine {
	t.Helper()
	u, err := trainingApp.NewPublishRoutine(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func publicationCommand(a security.AccessClaims, c routine.CreateCommand, rootRevision, draftRevision int64) routine.PublishCommand {
	p := routine.PublishPayload{DraftRevision: strconv.FormatInt(draftRevision, 10), VersionID: uuid.NewString(), Days: []routine.DayIDMap{}}
	for _, day := range c.Payload.Draft.Days {
		m := routine.DayIDMap{DraftDayID: day.ID, ID: uuid.NewString(), Exercises: []routine.ExerciseIDMap{}}
		for _, e := range day.Exercises {
			m.Exercises = append(m.Exercises, routine.ExerciseIDMap{DraftExerciseID: e.ID, ID: uuid.NewString()})
		}
		p.Days = append(p.Days, m)
	}
	return routine.PublishCommand{OperationID: uuid.NewString(), OriginDeviceID: a.DeviceID.String(), Type: routine.PublishCommandType, EntityID: c.EntityID, Base: protocol.Base{Revision: ptr(strconv.FormatInt(rootRevision, 10))}, CapturedAt: c.CapturedAt, Payload: p}
}
func completeRoutine(t *testing.T, base trainingApp.CreatePersonalExerciseDependencies, a security.AccessClaims) routine.CreateCommand {
	t.Helper()
	exercise := trainingCommand(a)
	if got, err := trainingUseCase(t, base).Execute(context.Background(), a, exercise); err != nil || got.Outcome != "accepted" {
		t.Fatal("exercise prerequisite failed", err)
	}
	c := routineCommand(a)
	c.Payload.Draft.Title = "  Rutina Élite  "
	for i := 0; i < 2; i++ {
		day := routine.DraftDay{ID: uuid.NewString(), Label: " Día ", Exercises: []routine.DraftExercise{}}
		for j := 0; j < 2; j++ {
			sets, rest := 3, 0
			ref := routine.ExerciseRef{Source: "personal", ID: exercise.EntityID}
			day.Exercises = append(day.Exercises, routine.DraftExercise{ID: uuid.NewString(), ExerciseRef: &ref, PrescribedSets: &sets, PrescribedReps: " 8–12 ", RestSeconds: &rest, Notes: "Nota <exacta>"})
		}
		c.Payload.Draft.Days = append(c.Payload.Draft.Days, day)
	}
	createDraftFixture(t, base, a, c)
	return c
}
func requirePublicationOutcome(t *testing.T, u *trainingApp.PublishRoutine, a security.AccessClaims, c routine.PublishCommand, outcome, code string) protocol.OperationResult {
	t.Helper()
	got, err := u.Execute(context.Background(), a, c)
	if err != nil || got.Outcome != outcome || (code != "" && (got.Error == nil || got.Error.Code != code)) {
		t.Fatal("unexpected publication outcome", got.Outcome, err)
	}
	return got
}
func readPublishedVersion(t *testing.T, d trainingApp.PublishRoutineDependencies, ownerID, versionID uuid.UUID) (*routine.Version, error) {
	t.Helper()
	tx, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), ownerID))
	if err != nil {
		return nil, err
	}
	defer shared.CloseQuery(tx)
	return d.Publications.GetVersion(tx, ownerID, versionID)
}

func TestRoutinePublishPostgresConcurrentReplayCompleteGroupAndHash(t *testing.T) {
	owner, a, base := trainingFixture(t)
	c := completeRoutine(t, base, a)
	d := publicationDependencies(base)
	u := publicationUseCase(t, d)
	command := publicationCommand(a, c, 1, 1)
	const count = 5
	results := make([]protocol.OperationResult, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = u.Execute(context.Background(), a, command)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Outcome != "accepted" || !reflect.DeepEqual(results[i], results[0]) {
			t.Fatal("publication replay diverged", errs[i])
		}
	}
	if len(results[0].Affected) != 3 {
		t.Fatal("publication omitted affected aggregate")
	}
	v, err := readPublishedVersion(t, d, a.AccountID, uuid.MustParse(command.Payload.VersionID))
	if err != nil {
		t.Fatal(err)
	}
	if v.VersionNumber != 1 || v.SourceDraftRevision != 1 || v.TitleSnapshot != c.Payload.Draft.Title || v.Days[0].Exercises[0].PrescribedReps != " 8–12 " || v.Days[0].Exercises[0].NameSnapshot != "  Press Élite 漢  " {
		t.Fatal("snapshot changed original values")
	}
	root, draft, err := routineRead(t, routineDependencies(base), a.AccountID, uuid.MustParse(c.EntityID))
	if err != nil || root.Revision != 2 || root.CurrentVersionID == nil || *root.CurrentVersionID != v.ID || root.Status != "ready" || draft.Revision != 2 || draft.Content != nil || draft.DeletedAt == nil || draft.BaseVersionID == nil || *draft.BaseVersionID != v.ID {
		t.Fatal("publication did not consume draft atomically", err)
	}
	for table, count := range map[string]int64{"routine_versions": 1, "routine_days": 2, "routine_exercises": 4} {
		if trainingCount(t, owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", a.AccountID) != count {
			t.Fatal("partial or duplicate version", table)
		}
	}
	var raw []byte
	if err = owner.Raw("SELECT changes FROM mancuerna.sync_changes WHERE account_id=? AND operation_id=?", a.AccountID, command.OperationID).Row().Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var changes []protocol.Change
	if json.Unmarshal(raw, &changes) != nil || len(changes) != 3 {
		t.Fatal("publication split across journal groups")
	}
	for _, change := range changes {
		var stored []byte
		if err = owner.Raw("SELECT canonical FROM mancuerna.record_revisions WHERE account_id=? AND entity_type=? AND entity_id=? AND revision=?", a.AccountID, change.EntityType, change.EntityID, change.Revision).Row().Scan(&stored); err != nil {
			t.Fatal(err)
		}
		x, _ := d.Canonicalizer.Canonicalize(change.Data)
		y, _ := d.Canonicalizer.Canonicalize(stored)
		if string(x) != string(y) {
			t.Fatal("journal differs from revision")
		}
		if change.EntityType == "routine_version" {
			dto, decodeErr := protocol.DecodeRoutineVersion(stored, d.Canonicalizer)
			if decodeErr != nil || dto.ID != v.ID.String() || dto.ContentHash != v.DTO().ContentHash {
				t.Fatal("persisted publication hash invalid", decodeErr)
			}
		}
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.sync_changes WHERE account_id=? AND operation_id=?", a.AccountID, command.OperationID) != 1 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.record_revisions WHERE account_id=? AND entity_type='routine_draft' AND entity_id=? AND revision=1", a.AccountID, c.EntityID) != 1 {
		t.Fatal("source revision lost or publication duplicated")
	}
	_, b, _ := trainingFixture(t)
	if _, err = readPublishedVersion(t, d, b.AccountID, v.ID); !errors.Is(err, trainingErrors.ErrRoutineNotFound) {
		t.Fatal("B read A version", err)
	}
	changed := command
	changed.CapturedAt = "2026-10-19T00:00:00.000Z"
	if _, err = u.Execute(context.Background(), a, changed); !errors.Is(err, protocol.ErrOperationMismatch) {
		t.Fatal("publication request hash normalized", err)
	}
}

type countPublicationMaterial struct {
	repository.RoutinePublicationRepository
	calls int
}

func (r *countPublicationMaterial) ResolveMaterial(tx shared.Transaction, owner uuid.UUID, d *routine.DraftRecord) (routine.PublicationMaterial, error) {
	r.calls++
	return r.RoutinePublicationRepository.ResolveMaterial(tx, owner, d)
}

func TestRoutinePublishPostgresBothCASAndDependenciesPrecedeSnapshots(t *testing.T) {
	owner, a, base := trainingFixture(t)
	c := completeRoutine(t, base, a)
	d := publicationDependencies(base)
	spy := &countPublicationMaterial{RoutinePublicationRepository: d.Publications}
	d.Publications = spy
	u := publicationUseCase(t, d)
	staleRoot := publicationCommand(a, c, 2, 1)
	rootConflict := requirePublicationOutcome(t, u, a, staleRoot, "conflict", "REVISION_CONFLICT")
	if spy.calls != 0 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.sync_conflicts WHERE id=? AND entity_type='routine' AND base_revision=2 AND current_revision=1", rootConflict.ConflictID) != 1 {
		t.Fatal("root CAS resolved material first")
	}
	save := draftCommand(a, c, 1)
	requireDraftOutcome(t, draftUseCase(t, draftDependencies(base)), a, save, "accepted", "")
	staleDraft := publicationCommand(a, c, 1, 1)
	draftConflict := requirePublicationOutcome(t, u, a, staleDraft, "conflict", "REVISION_CONFLICT")
	if spy.calls != 0 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.sync_conflicts WHERE id=? AND entity_type='routine_draft' AND base_revision=1 AND current_revision=2", draftConflict.ConflictID) != 1 {
		t.Fatal("draft CAS resolved material first")
	}
	var proposal []byte
	if err := owner.Raw("SELECT proposal FROM mancuerna.sync_conflicts WHERE id=?", draftConflict.ConflictID).Row().Scan(&proposal); err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(staleDraft)
	x, _ := d.Canonicalizer.Canonicalize(expected)
	y, _ := d.Canonicalizer.Canonicalize(proposal)
	if string(x) != string(y) {
		t.Fatal("publication conflict lost original request")
	}
	wrongParent := publicationCommand(a, c, 1, 2)
	wrongParent.Base = protocol.Base{AfterOperationID: &save.OperationID}
	requirePublicationOutcome(t, u, a, wrongParent, "rejected", "VALIDATION_ERROR")
	missing := publicationCommand(a, c, 1, 2)
	missing.Base = protocol.Base{AfterOperationID: ptr(uuid.NewString())}
	requirePublicationOutcome(t, u, a, missing, "dependency_pending", "DEPENDENCY_PENDING")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", missing.OperationID) != 0 {
		t.Fatal("missing dependency consumed operation")
	}
	blocked := publicationCommand(a, c, 1, 2)
	blocked.Base = protocol.Base{AfterOperationID: &wrongParent.OperationID}
	requirePublicationOutcome(t, u, a, blocked, "dependency_blocked", "DEPENDENCY_BLOCKED")
	if spy.calls != 0 {
		t.Fatal("invalid bases read snapshot sources")
	}
	valid := publicationCommand(a, c, 1, 2)
	valid.Base = protocol.Base{AfterOperationID: &c.OperationID}
	requirePublicationOutcome(t, u, a, valid, "accepted", "")
	if spy.calls != 1 {
		t.Fatal("valid publication skipped material")
	}
	if replay := requirePublicationOutcome(t, u, a, staleDraft, "conflict", "REVISION_CONFLICT"); !reflect.DeepEqual(replay, draftConflict) {
		t.Fatal("old conflict changed after publication")
	}
}

func TestRoutinePublishPostgresGlobalIdentityCollisionsRollbackSavepoint(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	d := publicationDependencies(base)
	u := publicationUseCase(t, d)
	known := []routine.PublishCommand{}
	for _, actor := range []security.AccessClaims{a, b} {
		c := completeRoutine(t, base, actor)
		p := publicationCommand(actor, c, 1, 1)
		requirePublicationOutcome(t, u, actor, p, "accepted", "")
		known = append(known, p)
	}
	for _, prior := range known {
		for _, kind := range []string{"version", "day", "exercise"} {
			c := completeRoutine(t, base, a)
			p := publicationCommand(a, c, 1, 1)
			switch kind {
			case "version":
				p.Payload.VersionID = prior.Payload.VersionID
			case "day":
				p.Payload.Days[1].ID = prior.Payload.Days[0].ID
			case "exercise":
				p.Payload.Days[1].Exercises[1].ID = prior.Payload.Days[0].Exercises[0].ID
			}
			before := trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID)
			first := requirePublicationOutcome(t, u, a, p, "rejected", "REVISION_CONFLICT")
			if replay := requirePublicationOutcome(t, u, a, p, "rejected", "REVISION_CONFLICT"); !reflect.DeepEqual(first, replay) {
				t.Fatal("identity collision receipt changed")
			}
			if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.routine_versions WHERE account_id=? AND routine_id=?", a.AccountID, c.EntityID) != 0 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != before {
				t.Fatal("collision left partial publication", kind)
			}
			root, draft, err := routineRead(t, routineDependencies(base), a.AccountID, uuid.MustParse(c.EntityID))
			if err != nil || root.Revision != 1 || root.Status != "draft" || root.CurrentVersionID != nil || draft.Revision != 1 || draft.Content == nil {
				t.Fatal("savepoint did not restore editorial state", err)
			}
		}
	}
	// Concurrent accounts sharing a destination child have one winner. Neither
	// receives the owner's identity, and the loser can persist its rejection.
	ca, cb := completeRoutine(t, base, a), completeRoutine(t, base, b)
	pa, pb := publicationCommand(a, ca, 1, 1), publicationCommand(b, cb, 1, 1)
	pb.Payload.Days[0].Exercises[0].ID = pa.Payload.Days[0].Exercises[0].ID
	results := make([]protocol.OperationResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, v := range []struct {
		actor   security.AccessClaims
		command routine.PublishCommand
	}{{a, pa}, {b, pb}} {
		wg.Add(1)
		go func(i int, v struct {
			actor   security.AccessClaims
			command routine.PublishCommand
		}) {
			defer wg.Done()
			results[i], errs[i] = u.Execute(context.Background(), v.actor, v.command)
		}(i, v)
	}
	wg.Wait()
	wins := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatal("concurrent identity collision failed transaction", errs[i])
		}
		if r.Outcome == "accepted" {
			wins++
		} else if r.Outcome != "rejected" || r.Error.Code != "REVISION_CONFLICT" {
			t.Fatal("unexpected identity collision result")
		}
	}
	if wins != 1 {
		t.Fatal("shared child identity accepted twice")
	}
}

type failAfterPublication struct {
	repository.RoutinePublicationRepository
}

func (r failAfterPublication) Publish(tx shared.Transaction, owner uuid.UUID, p *routine.Publication) (bool, error) {
	ok, err := r.RoutinePublicationRepository.Publish(tx, owner, p)
	if err != nil {
		return false, err
	}
	if ok {
		return false, errors.New("synthetic failure after complete publication")
	}
	return false, nil
}

func TestRoutinePublishPostgresRollbackAndDatabaseImmutability(t *testing.T) {
	owner, a, base := trainingFixture(t)
	c := completeRoutine(t, base, a)
	d := publicationDependencies(base)
	p := publicationCommand(a, c, 1, 1)
	before := trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID)
	for _, mode := range []int{0, 1} {
		broken := d
		if mode == 0 {
			broken.Publications = failAfterPublication{d.Publications}
		} else {
			broken.Commands = trainingFailCommit{d.Commands}
		}
		if _, err := publicationUseCase(t, broken).Execute(context.Background(), a, p); err == nil {
			t.Fatal("injected failure ignored")
		}
		if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.routine_versions WHERE account_id=?", a.AccountID) != 0 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.operation_receipts WHERE operation_id=?", p.OperationID) != 0 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != before {
			t.Fatal("publication escaped rollback")
		}
		root, draft, err := routineRead(t, routineDependencies(base), a.AccountID, uuid.MustParse(c.EntityID))
		if err != nil || root.Revision != 1 || draft.Revision != 1 || draft.Content == nil {
			t.Fatal("rollback consumed draft", err)
		}
	}
	requirePublicationOutcome(t, publicationUseCase(t, d), a, p, "accepted", "")
	queries := []struct {
		sql  string
		args []any
	}{
		{"UPDATE mancuerna.routine_versions SET sealed=false WHERE account_id=? AND id=?", []any{a.AccountID, p.Payload.VersionID}},
		{"UPDATE mancuerna.routine_versions SET title_snapshot='tampered' WHERE account_id=? AND id=?", []any{a.AccountID, p.Payload.VersionID}},
		{"UPDATE mancuerna.routine_exercises SET name_snapshot='tampered' WHERE account_id=? AND version_id=?", []any{a.AccountID, p.Payload.VersionID}},
		{"DELETE FROM mancuerna.routine_days WHERE account_id=? AND version_id=?", []any{a.AccountID, p.Payload.VersionID}},
		{"INSERT INTO mancuerna.routine_days(id,account_id,version_id,label,position) VALUES(?,?,?,'Injected',2)", []any{uuid.New(), a.AccountID, p.Payload.VersionID}},
	}
	for _, q := range queries {
		err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), a.AccountID), func(tx shared.Transaction) error { return tx.(*shared.GormTransaction).Tx.Exec(q.sql, q.args...).Error })
		if err == nil {
			t.Fatal("runtime mutated sealed history")
		}
	}
	if _, err := readPublishedVersion(t, d, a.AccountID, uuid.MustParse(p.Payload.VersionID)); err != nil {
		t.Fatal("failed mutations corrupted immutable version", err)
	}
	// The deferred database guard forbids committing an unsealed/incomplete
	// version even when a low-level caller omits the publication repository.
	err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), a.AccountID), func(tx shared.Transaction) error {
		return tx.(*shared.GormTransaction).Tx.Exec("INSERT INTO mancuerna.routine_versions(id,account_id,routine_id,version_number,title_snapshot,content_hash,source_draft_revision,actor_device_id) VALUES(?,?,?,2,'Incomplete',decode(repeat('01',32),'hex'),2,?)", uuid.New(), a.AccountID, c.EntityID, a.DeviceID).Error
	})
	if err == nil {
		t.Fatal("incomplete version committed")
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.routine_versions WHERE account_id=?", a.AccountID) != 1 {
		t.Fatal("incomplete version persisted")
	}
}

func editFromVersion(t *testing.T, base trainingApp.CreatePersonalExerciseDependencies, a security.AccessClaims, source *routine.Version, draftRevision int64) routine.CreateCommand {
	t.Helper()
	c := routineCommand(a)
	c.EntityID = source.RoutineID.String()
	c.Payload.Draft = routine.Draft{FormatVersion: 1, Title: source.TitleSnapshot, Days: []routine.DraftDay{}}
	for _, day := range source.Days {
		d := routine.DraftDay{ID: day.ID.String(), Label: day.Label, Exercises: []routine.DraftExercise{}}
		for _, e := range day.Exercises {
			ref, sets := e.Reference, e.PrescribedSets
			d.Exercises = append(d.Exercises, routine.DraftExercise{ID: e.ID.String(), ExerciseRef: &ref, PrescribedSets: &sets, PrescribedReps: e.PrescribedReps, RestSeconds: e.RestSeconds, Notes: e.Notes})
		}
		c.Payload.Draft.Days = append(c.Payload.Draft.Days, d)
	}
	save := draftCommand(a, c, draftRevision)
	save.Payload.BaseVersionID = ptr(source.ID.String())
	requireDraftOutcome(t, draftUseCase(t, draftDependencies(base)), a, save, "accepted", "")
	return c
}

func TestRoutinePublishPostgresRenameArchiveAndExactHistoricalSnapshots(t *testing.T) {
	owner, a, base := trainingFixture(t)
	c := completeRoutine(t, base, a)
	d := publicationDependencies(base)
	u := publicationUseCase(t, d)
	p1 := publicationCommand(a, c, 1, 1)
	requirePublicationOutcome(t, u, a, p1, "accepted", "")
	v1, err := readPublishedVersion(t, d, a.AccountID, uuid.MustParse(p1.Payload.VersionID))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(v1.DTO())
	exerciseID := v1.Days[0].Exercises[0].Reference.ID
	if err = owner.Exec("UPDATE mancuerna.personal_exercises SET name='Renamed active',load_mode='none',revision=revision+1 WHERE account_id=? AND id=?", a.AccountID, exerciseID).Error; err != nil {
		t.Fatal(err)
	}
	c2 := editFromVersion(t, base, a, v1, 2)
	p2 := publicationCommand(a, c2, 2, 3)
	requirePublicationOutcome(t, u, a, p2, "accepted", "")
	v2, err := readPublishedVersion(t, d, a.AccountID, uuid.MustParse(p2.Payload.VersionID))
	if err != nil || v2.VersionNumber != 2 || v2.Days[0].Exercises[0].NameSnapshot != "Renamed active" || v2.Days[0].Exercises[0].LoadModeSnapshot != "none" {
		t.Fatal("active snapshot did not freeze current values", err)
	}
	if err = owner.Exec("UPDATE mancuerna.personal_exercises SET name='Withdrawn current',load_mode='external',archived_at=clock_timestamp(),deleted_at=clock_timestamp(),revision=revision+1 WHERE account_id=? AND id=?", a.AccountID, exerciseID).Error; err != nil {
		t.Fatal(err)
	}
	c3 := editFromVersion(t, base, a, v2, 4)
	p3 := publicationCommand(a, c3, 3, 5)
	requirePublicationOutcome(t, u, a, p3, "accepted", "")
	v3, err := readPublishedVersion(t, d, a.AccountID, uuid.MustParse(p3.Payload.VersionID))
	if err != nil || v3.VersionNumber != 3 || v3.Days[0].Exercises[0].NameSnapshot != "Renamed active" || v3.Days[0].Exercises[0].LoadModeSnapshot != "none" {
		t.Fatal("retired reference did not use sealed historical snapshot", err)
	}
	v1After, err := readPublishedVersion(t, d, a.AccountID, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(v1After.DTO())
	if string(original) != string(after) {
		t.Fatal("new version rewrote original history")
	}
	// A persisted draft alone does not contain an immutable name/mode snapshot.
	other := completeRoutine(t, base, a)
	retired := other.Payload.Draft.Days[0].Exercises[0].ExerciseRef.ID
	if err = owner.Exec("UPDATE mancuerna.personal_exercises SET archived_at=clock_timestamp() WHERE account_id=? AND id=?", a.AccountID, retired).Error; err != nil {
		t.Fatal(err)
	}
	bad := publicationCommand(a, other, 1, 1)
	requirePublicationOutcome(t, u, a, bad, "rejected", "VALIDATION_ERROR")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.routine_versions WHERE account_id=? AND routine_id=?", a.AccountID, other.EntityID) != 0 || trainingCount(t, owner, "SELECT revision FROM mancuerna.routine_drafts WHERE routine_id=?", other.EntityID) != 1 {
		t.Fatal("retired unsnapshotted draft was consumed")
	}
	// A new editorial appearance may be saved while its reference is active,
	// but cannot borrow another occurrence's snapshot after that reference retires.
	fresh := completeRoutine(t, base, a)
	freshPublish := publicationCommand(a, fresh, 1, 1)
	requirePublicationOutcome(t, u, a, freshPublish, "accepted", "")
	freshVersion, err := readPublishedVersion(t, d, a.AccountID, uuid.MustParse(freshPublish.Payload.VersionID))
	if err != nil {
		t.Fatal(err)
	}
	changed := editFromVersion(t, base, a, freshVersion, 2)
	changed.Payload.Draft.Days[0].Exercises[0].ID = uuid.NewString()
	save := draftCommand(a, changed, 3)
	save.Payload.BaseVersionID = ptr(freshVersion.ID.String())
	requireDraftOutcome(t, draftUseCase(t, draftDependencies(base)), a, save, "accepted", "")
	if err = owner.Exec("UPDATE mancuerna.personal_exercises SET archived_at=clock_timestamp() WHERE account_id=? AND id=?", a.AccountID, freshVersion.Days[0].Exercises[0].Reference.ID).Error; err != nil {
		t.Fatal(err)
	}
	requirePublicationOutcome(t, u, a, publicationCommand(a, changed, 2, 4), "rejected", "VALIDATION_ERROR")
}

func clonePublicationCommand(t *testing.T, c routine.PublishCommand) routine.PublishCommand {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out routine.PublishCommand
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestRoutinePublishPostgresValidationScopeAndWithdrawnActor(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	c := completeRoutine(t, base, a)
	d := publicationDependencies(base)
	u := publicationUseCase(t, d)
	p := publicationCommand(a, c, 1, 1)
	for _, edit := range []func(*routine.PublishCommand){func(p *routine.PublishCommand) { p.Payload.Days = p.Payload.Days[:1] }, func(p *routine.PublishCommand) { p.Payload.Days[1].ID = p.Payload.Days[0].ID }, func(p *routine.PublishCommand) { p.Payload.Days[0].DraftDayID = uuid.NewString() }, func(p *routine.PublishCommand) { p.Payload.Days[0].Exercises[0].ID = p.Payload.VersionID }} {
		bad := clonePublicationCommand(t, p)
		bad.OperationID = uuid.NewString()
		edit(&bad)
		requirePublicationOutcome(t, u, a, bad, "rejected", "VALIDATION_ERROR")
	}
	foreign := p
	foreign.OperationID = uuid.NewString()
	foreign.OriginDeviceID = b.DeviceID.String()
	requirePublicationOutcome(t, u, b, foreign, "rejected", "NOT_FOUND")
	for _, ctx := range []context.Context{shared.WithPersonalAccount(context.Background(), b.AccountID), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := u.Execute(ctx, a, p); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed scope published", err)
		}
	}
	if err := owner.Exec("UPDATE mancuerna.routines SET status='archived' WHERE account_id=? AND id=?", a.AccountID, c.EntityID).Error; err != nil {
		t.Fatal(err)
	}
	archived := clonePublicationCommand(t, p)
	archived.OperationID = uuid.NewString()
	requirePublicationOutcome(t, u, a, archived, "rejected", "REVISION_CONFLICT")
	if err := owner.Exec("UPDATE mancuerna.routines SET status='draft' WHERE account_id=? AND id=?", a.AccountID, c.EntityID).Error; err != nil {
		t.Fatal(err)
	}
	deleted := completeRoutine(t, base, a)
	if err := owner.Exec("UPDATE mancuerna.routines SET deleted_at=clock_timestamp() WHERE account_id=? AND id=?", a.AccountID, deleted.EntityID).Error; err != nil {
		t.Fatal(err)
	}
	deletedResult := requirePublicationOutcome(t, u, a, publicationCommand(a, deleted, 1, 1), "conflict", "ENTITY_DELETED")
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.sync_conflicts WHERE id=? AND entity_type='routine' AND reason='deleted'", deletedResult.ConflictID) != 1 || trainingCount(t, owner, "SELECT count(*) FROM mancuerna.routine_versions WHERE account_id=? AND routine_id=?", a.AccountID, deleted.EntityID) != 0 {
		t.Fatal("deleted root published or lost proposal")
	}
	authUoW, err := shared.NewPersonalPostgresUnitOfWork(trainingTestPool(t, "MANCUERNA_TEST_AUTH_DSN"), shared.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	wrongPool := d
	wrongPool.UoW = authUoW
	if _, err = publicationUseCase(t, wrongPool).Execute(context.Background(), a, p); err == nil {
		t.Fatal("auth pool published training data")
	}
	requirePublicationOutcome(t, u, a, p, "accepted", "")
	if err := owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", a.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(context.Background(), a, p); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("withdrawn actor replayed publication", err)
	}
}

func TestRoutinePublishPostgresVersionAndGroupSizeLimitsRemainTerminal(t *testing.T) {
	owner, a, base := trainingFixture(t)
	exercise := trainingCommand(a)
	exercise.Payload.Name = strings.Repeat("😀", 120)
	if got, err := trainingUseCase(t, base).Execute(context.Background(), a, exercise); err != nil || got.Outcome != "accepted" {
		t.Fatal(err)
	}
	c := routineCommand(a)
	c.Payload.Draft.Title = "Large complete version"
	for i := 0; i < 14; i++ {
		day := routine.DraftDay{ID: uuid.NewString(), Label: "Day", Exercises: []routine.DraftExercise{}}
		for j := 0; j < 50; j++ {
			sets := 1
			ref := routine.ExerciseRef{Source: "personal", ID: exercise.EntityID}
			day.Exercises = append(day.Exercises, routine.DraftExercise{ID: uuid.NewString(), ExerciseRef: &ref, PrescribedSets: &sets, PrescribedReps: "1", Notes: strings.Repeat("a", 900)})
		}
		c.Payload.Draft.Days = append(c.Payload.Draft.Days, day)
	}
	createDraftFixture(t, base, a, c)
	d := publicationDependencies(base)
	u := publicationUseCase(t, d)
	p := publicationCommand(a, c, 1, 1)
	before := trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID)
	first := requirePublicationOutcome(t, u, a, p, "rejected", "LIMIT_EXCEEDED")
	if replay := requirePublicationOutcome(t, u, a, p, "rejected", "LIMIT_EXCEEDED"); !reflect.DeepEqual(first, replay) {
		t.Fatal("oversize version result changed")
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.routine_versions WHERE account_id=?", a.AccountID) != 0 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != before || trainingCount(t, owner, "SELECT revision FROM mancuerna.routine_drafts WHERE routine_id=?", c.EntityID) != 1 {
		t.Fatal("oversize version partially published")
	}
	// Now make the version itself fit while the full three-change group does
	// not. Bytes are measured using the real producer and canonicalizer.
	material := routine.PublicationMaterial{NextVersionNumber: 1, Exercises: map[string]routine.ExerciseSnapshot{}}
	for i := range c.Payload.Draft.Days {
		for j := range c.Payload.Draft.Days[i].Exercises {
			e := &c.Payload.Draft.Days[i].Exercises[j]
			e.Notes = ""
			material.Exercises[e.ID] = routine.ExerciseSnapshot{Reference: *e.ExerciseRef, Name: exercise.Payload.Name, LoadMode: string(exercise.Payload.LoadMode)}
		}
	}
	now := base.Now().UTC()
	root, draft, err := routine.NewDraftRoutine(uuid.MustParse(c.EntityID), a.AccountID, c.Payload.Draft, now)
	if err != nil {
		t.Fatal(err)
	}
	draft.Revision = 2
	next := publicationCommand(a, c, 1, 2)
	measure := func(content routine.Draft) int {
		t.Helper()
		draft.Content = &content
		publication, err := routine.NewPublication(root, draft, next.Payload, material, a.DeviceID, now, d.Canonicalizer)
		if err != nil {
			t.Fatal("group fixture construction failed", err)
		}
		raw, _ := json.Marshal(publication.Version.DTO())
		canonical, err := d.Canonicalizer.Canonicalize(raw)
		if err != nil {
			t.Fatal(err)
		}
		return len(canonical)
	}
	const target = 1048576 - 128
	remaining := target - measure(c.Payload.Draft)
	for i := range c.Payload.Draft.Days {
		for j := range c.Payload.Draft.Days[i].Exercises {
			n := remaining
			if n > 1000 {
				n = 1000
			}
			if n < 0 {
				t.Fatal("invalid version size fixture")
			}
			c.Payload.Draft.Days[i].Exercises[j].Notes = strings.Repeat("a", n)
			remaining -= n
		}
	}
	if remaining != 0 || measure(c.Payload.Draft) != target {
		t.Fatal("version did not reach exact canonical bound")
	}
	save := draftCommand(a, c, 1)
	requireDraftOutcome(t, draftUseCase(t, draftDependencies(base)), a, save, "accepted", "")
	first = requirePublicationOutcome(t, u, a, next, "rejected", "LIMIT_EXCEEDED")
	if replay := requirePublicationOutcome(t, u, a, next, "rejected", "LIMIT_EXCEEDED"); !reflect.DeepEqual(first, replay) {
		t.Fatal("group size rejection not durable")
	}
	if trainingCount(t, owner, "SELECT count(*) FROM mancuerna.routine_versions WHERE account_id=?", a.AccountID) != 0 || trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != before+1 || trainingCount(t, owner, "SELECT revision FROM mancuerna.routine_drafts WHERE routine_id=?", c.EntityID) != 2 {
		t.Fatal("oversize group partially published")
	}
}
