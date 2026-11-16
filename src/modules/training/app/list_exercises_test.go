package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	cursorSecurity "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/google/uuid"
)

type exerciseReadList struct {
	rows         []*exercise.ListedExercise
	after        *exercise.ListPosition
	filter       exercise.ListFilter
	owner        uuid.UUID
	limit, calls int
}

func (r *exerciseReadList) List(_ shared.Transaction, owner uuid.UUID, filter exercise.ListFilter, after *exercise.ListPosition, limit int) ([]*exercise.ListedExercise, error) {
	r.calls++
	r.owner = owner
	r.filter = filter
	r.after = after
	r.limit = limit
	return r.rows, nil
}
func exerciseReaderFixture(t *testing.T) (security.AccessClaims, ListExercisesDependencies, *routineReadUoW, *routineReadCommands, *routineReadStream, *exerciseReadList, *time.Time) {
	t.Helper()
	a := security.AccessClaims{AccountID: uuid.New(), DeviceID: uuid.New(), SessionID: uuid.New()}
	now := time.Date(2026, 10, 23, 12, 0, 0, 123000000, time.UTC)
	uow, commands, stream := &routineReadUoW{actor: a.AccountID}, &routineReadCommands{}, &routineReadStream{epoch: uuid.New()}
	e := &exercise.ListedExercise{Exercise: exercise.Exercise{ID: uuid.New(), Name: "Straße", LoadMode: exercise.External, Notes: "Original\nnotes", Revision: 9007199254740993, CreatedAt: now, UpdatedAt: now}, Source: exercise.CatalogSource}
	p := *e
	p.Source, p.AccountID = exercise.PersonalSource, a.AccountID
	rows := &exerciseReadList{rows: []*exercise.ListedExercise{e, &p}}
	signer, err := cursorSecurity.NewCursorSigner("test", map[string][]byte{"test": []byte(strings.Repeat("e", 32))})
	if err != nil {
		t.Fatal(err)
	}
	d := ListExercisesDependencies{UoW: uow, Commands: commands, Stream: stream, Exercises: rows, Cursors: signer, Now: func() time.Time { return now }}
	return a, d, uow, commands, stream, rows, &now
}

func TestExerciseListCursorPreservesSourceCollisionAndBindsFilters(t *testing.T) {
	a, d, uow, _, stream, rows, now := exerciseReaderFixture(t)
	u, err := NewListExercises(d)
	if err != nil {
		t.Fatal(err)
	}
	page, err := u.Execute(context.Background(), a, ListExercisesInput{Query: " STRASSE\t", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Source != "catalog" || page.NextCursor == nil || rows.limit != 2 || rows.owner != a.AccountID {
		t.Fatal("first page", err)
	}
	encoded, _ := json.Marshal(page.Items[0])
	var wire map[string]any
	_ = json.Unmarshal(encoded, &wire)
	if len(wire) != 10 || wire["revision"] != "9007199254740993" || wire["name"] != "Straße" || wire["notes"] != "Original\nnotes" || wire["deleted_at"] != nil || wire["archived_at"] != nil {
		t.Fatal("closed exercise projection changed", wire)
	}
	rows.rows = rows.rows[1:]
	next, err := u.Execute(context.Background(), a, ListExercisesInput{Query: "straße", Cursor: *page.NextCursor, Limit: 100})
	if err != nil || len(next.Items) != 1 || next.Items[0].Source != "personal" || next.Items[0].ID != page.Items[0].ID || next.NextCursor != nil || rows.after.Source != "catalog" || rows.filter.Query != "strasse" {
		t.Fatal("cross-source collision pagination", err)
	}
	for _, in := range []ListExercisesInput{{Query: "otro", Cursor: *page.NextCursor}, {Query: "strasse", Cursor: *page.NextCursor + "x"}} {
		calls := rows.calls
		if out, err := u.Execute(context.Background(), a, in); !errors.Is(err, protocol.ErrCursorInvalid) || out.Items != nil || rows.calls != calls {
			t.Fatal("changed cursor reached repository", err)
		}
	}
	other := a
	other.AccountID = uuid.New()
	uow.actor = other.AccountID
	if _, err := u.Execute(context.Background(), other, ListExercisesInput{Query: "strasse", Cursor: *page.NextCursor}); !errors.Is(err, protocol.ErrCursorInvalid) {
		t.Fatal("cross-account cursor", err)
	}
	uow.actor = a.AccountID
	*now = now.Add(25 * time.Hour)
	if _, err := u.Execute(context.Background(), a, ListExercisesInput{Query: "strasse", Cursor: *page.NextCursor}); !errors.Is(err, protocol.ErrCursorExpired) {
		t.Fatal("expired cursor", err)
	}
	*now = now.Add(-25 * time.Hour)
	stream.epoch = uuid.New()
	if _, err := u.Execute(context.Background(), a, ListExercisesInput{Query: "strasse", Cursor: *page.NextCursor}); !errors.Is(err, protocol.ErrCursorEpochChanged) {
		t.Fatal("changed epoch", err)
	}
}

func TestExerciseListRejectsScopeAndRevocationBeforeCatalog(t *testing.T) {
	a, d, uow, commands, _, rows, _ := exerciseReaderFixture(t)
	u, _ := NewListExercises(d)
	for _, ctx := range []context.Context{shared.WithTenant(context.Background(), uuid.New()), shared.WithPersonalAccount(context.Background(), uuid.New()), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1})} {
		if _, err := u.Execute(ctx, a, ListExercisesInput{}); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed context", err)
		}
	}
	bad := a
	bad.SessionID = uuid.Nil
	if _, err := u.Execute(context.Background(), bad, ListExercisesInput{}); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("bad actor", err)
	}
	for _, in := range []ListExercisesInput{{Limit: -1}, {Limit: 101}, {Query: strings.Repeat("😀", 121)}, {Query: "\x00"}, {Cursor: strings.Repeat("x", protocol.CursorMaximumLength+1)}} {
		if _, err := u.Execute(context.Background(), a, in); !errors.Is(err, trainingErrors.ErrInvalidExercise) {
			t.Fatal("invalid query", err)
		}
	}
	if uow.calls != 0 {
		t.Fatal("invalid input reached UoW")
	}
	commands.err = protocol.ErrCommandActor
	if out, err := u.Execute(context.Background(), a, ListExercisesInput{}); !errors.Is(err, protocol.ErrCommandActor) || out.Items != nil || rows.calls != 0 {
		t.Fatal("revoked session read global catalog", err)
	}
}

func TestExerciseListRejectsHiddenUnorderedOrMismatchedProjection(t *testing.T) {
	a, d, _, _, _, rows, now := exerciseReaderFixture(t)
	u, _ := NewListExercises(d)
	valid := *rows.rows[0]
	for _, change := range []func(*exercise.ListedExercise){
		func(e *exercise.ListedExercise) { e.Source = "personal"; e.AccountID = uuid.New() },
		func(e *exercise.ListedExercise) { e.AccountID = a.AccountID },
		func(e *exercise.ListedExercise) { e.ArchivedAt = now },
		func(e *exercise.ListedExercise) { e.DeletedAt = now },
		func(e *exercise.ListedExercise) { e.Name = "other" },
		func(e *exercise.ListedExercise) { e.Revision = 0 },
	} {
		bad := valid
		change(&bad)
		rows.rows = []*exercise.ListedExercise{&bad}
		if out, err := u.Execute(context.Background(), a, ListExercisesInput{Query: "strasse"}); !errors.Is(err, trainingErrors.ErrTrainingPersistence) || out.Items != nil {
			t.Fatal("hidden/invalid projection escaped", err)
		}
	}
	for _, bad := range [][]*exercise.ListedExercise{{nil}, {&valid, &valid}, make([]*exercise.ListedExercise, 102)} {
		rows.rows = bad
		if _, err := u.Execute(context.Background(), a, ListExercisesInput{Limit: 100}); !errors.Is(err, trainingErrors.ErrTrainingPersistence) {
			t.Fatal("invalid repository result", err)
		}
	}
	rows.rows = nil
	out, err := u.Execute(context.Background(), a, ListExercisesInput{Query: strings.Repeat("ﬃ", 120)})
	if err != nil || out.Items == nil || len(out.Items) != 0 || out.NextCursor != nil || rows.filter.Query != strings.Repeat("ffi", 120) || rows.limit != 51 {
		t.Fatal("empty array/default/expanded search", err)
	}
}
