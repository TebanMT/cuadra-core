package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	protocolRepository "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	cursorSecurity "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/google/uuid"
)

type routineReadUoW struct {
	shared.UnitOfWork
	actor uuid.UUID
	calls int
}

func (u *routineReadUoW) Command(ctx context.Context, fn func(shared.Transaction) error) error {
	u.calls++
	if shared.PersonalAccountID(ctx) != u.actor {
		return shared.ErrPersonalScope
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("missing timeout")
	}
	return fn(nil)
}

type routineReadCommands struct {
	protocolRepository.Commands
	calls int
	err   error
}

func (c *routineReadCommands) RequireActor(_ shared.Transaction, actor protocol.Actor, origin uuid.UUID) error {
	c.calls++
	if origin != actor.DeviceID {
		return protocol.ErrCommandOrigin
	}
	return c.err
}

type routineReadStream struct {
	protocolRepository.Stream
	epoch uuid.UUID
}

func (s *routineReadStream) State(shared.Transaction, uuid.UUID) (protocol.StreamState, error) {
	return protocol.StreamState{Epoch: s.epoch}, nil
}

type routineReadRepository struct {
	repository.RoutineRepository
	root  *routine.Routine
	draft *routine.DraftRecord
	calls int
}

func (r *routineReadRepository) GetByID(shared.Transaction, uuid.UUID, uuid.UUID) (*routine.Routine, error) {
	r.calls++
	return r.root, nil
}
func (r *routineReadRepository) GetDraftByID(shared.Transaction, uuid.UUID, uuid.UUID) (*routine.DraftRecord, error) {
	r.calls++
	return r.draft, nil
}

type routineReadList struct {
	rows   []*routine.Routine
	after  *routine.ListPosition
	filter routine.ListFilter
	calls  int
}

func (r *routineReadList) List(_ shared.Transaction, _ uuid.UUID, filter routine.ListFilter, after *routine.ListPosition, limit int) ([]*routine.Routine, error) {
	r.calls++
	r.after = after
	r.filter = filter
	return r.rows, nil
}

func routineReaderFixture(t *testing.T) (security.AccessClaims, *routineReadUoW, *routineReadCommands, *routineReadRepository, time.Time) {
	t.Helper()
	a := security.AccessClaims{AccountID: uuid.New(), SessionID: uuid.New(), DeviceID: uuid.New()}
	at := time.Date(2026, 10, 23, 12, 0, 0, 123456000, time.UTC)
	r, d, err := routine.NewDraftRoutine(uuid.New(), a.AccountID, routine.Draft{FormatVersion: 1, Title: "PIÉRNA", Days: []routine.DraftDay{}}, at)
	if err != nil {
		t.Fatal(err)
	}
	return a, &routineReadUoW{actor: a.AccountID}, &routineReadCommands{}, &routineReadRepository{root: r, draft: d}, at
}
func TestRoutineReadRejectsScopeAndRevocationBeforeData(t *testing.T) {
	a, uow, commands, repo, _ := routineReaderFixture(t)
	u, _ := NewGetRoutine(RoutineReadDependencies{UoW: uow, Commands: commands, Routines: repo})
	for _, ctx := range []context.Context{shared.WithTenant(context.Background(), uuid.New()), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithPersonalAccount(context.Background(), uuid.New())} {
		if _, err := u.Execute(ctx, a, repo.root.ID); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed scope", err)
		}
	}
	bad := a
	bad.SessionID = uuid.Nil
	if _, err := u.Execute(context.Background(), bad, repo.root.ID); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("bad actor", err)
	}
	if uow.calls != 0 {
		t.Fatal("invalid scope reached transaction")
	}
	commands.err = protocol.ErrCommandActor
	if out, err := u.Execute(context.Background(), a, repo.root.ID); !errors.Is(err, protocol.ErrCommandActor) || out.ID != "" || repo.calls != 0 {
		t.Fatal("revoked actor reached data", err)
	}
}
func TestRoutineReadPreservesBigintAndDraftTombstone(t *testing.T) {
	a, uow, commands, repo, at := routineReaderFixture(t)
	d := RoutineReadDependencies{UoW: uow, Commands: commands, Routines: repo}
	u, _ := NewGetRoutine(d)
	draft, _ := NewGetRoutineDraft(d)
	repo.root.Revision = 9007199254740993
	out, err := u.Execute(context.Background(), a, repo.root.ID)
	if err != nil || out.Revision != "9007199254740993" || out.Title != "PIÉRNA" {
		t.Fatal("root transport", err)
	}
	base := uuid.New()
	repo.draft.Revision = 9007199254740994
	repo.draft.Content = nil
	repo.draft.BaseVersionID = &base
	repo.draft.DeletedAt = &at
	got, err := draft.Execute(context.Background(), a, repo.root.ID)
	if err != nil || got.Content != nil || got.DeletedAt == nil || got.BaseVersionID == nil || *got.BaseVersionID != base.String() || got.Revision != "9007199254740994" {
		t.Fatal("publication tombstone lost", err)
	}
	repo.root.AccountID = uuid.New()
	if got, err := u.Execute(context.Background(), a, repo.root.ID); !errors.Is(err, trainingErrors.ErrTrainingPersistence) || got.ID != "" {
		t.Fatal("foreign row escaped", err)
	}
}
func TestRoutineListCursorBindsSemanticFiltersEpochOwnerAndMicroseconds(t *testing.T) {
	a, uow, commands, repo, now := routineReaderFixture(t)
	first := *repo.root
	first.CreatedAt = now
	first.UpdatedAt = now
	second := first
	second.ID = uuid.New()
	second.CreatedAt = now.Add(-time.Microsecond)
	second.UpdatedAt = second.CreatedAt
	rows := &routineReadList{rows: []*routine.Routine{&first, &second}}
	stream := &routineReadStream{epoch: uuid.New()}
	signer, err := cursorSecurity.NewCursorSigner("test", map[string][]byte{"test": []byte(strings.Repeat("s", 32))})
	if err != nil {
		t.Fatal(err)
	}
	d := ListRoutinesDependencies{UoW: uow, Commands: commands, Stream: stream, Routines: rows, Cursors: signer, Now: func() time.Time { return now }}
	u, _ := NewListRoutines(d)
	page, err := u.Execute(context.Background(), a, ListRoutinesInput{Query: "  piÉrna\t", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatal("first page", err)
	}
	rows.rows = []*routine.Routine{&second}
	next, err := u.Execute(context.Background(), a, ListRoutinesInput{Query: "PIERNA", Status: "active", Cursor: *page.NextCursor, Limit: 100})
	if err != nil || len(next.Items) != 1 || next.NextCursor != nil || !rows.after.CreatedAt.Equal(first.CreatedAt) || rows.filter.Query != "pierna" {
		t.Fatal("equivalent filter / microsecond page", err)
	}
	for _, change := range []ListRoutinesInput{{Cursor: *page.NextCursor, Query: "otro"}, {Cursor: *page.NextCursor, Query: "pierna", Status: "all"}, {Cursor: *page.NextCursor + "x", Query: "pierna"}} {
		before := rows.calls
		if got, err := u.Execute(context.Background(), a, change); !errors.Is(err, protocol.ErrCursorInvalid) || got.Items != nil || rows.calls != before {
			t.Fatal("changed filter or token accepted", err)
		}
	}
	other := a
	other.AccountID = uuid.New()
	uow.actor = other.AccountID
	if _, err := u.Execute(context.Background(), other, ListRoutinesInput{Query: "pierna", Cursor: *page.NextCursor}); !errors.Is(err, protocol.ErrCursorInvalid) {
		t.Fatal("cursor crosses account", err)
	}
	uow.actor = a.AccountID
	now = now.Add(25 * time.Hour)
	if _, err := u.Execute(context.Background(), a, ListRoutinesInput{Query: "pierna", Cursor: *page.NextCursor}); !errors.Is(err, protocol.ErrCursorExpired) {
		t.Fatal("expired cursor accepted", err)
	}
	now = now.Add(-25 * time.Hour)
	stream.epoch = uuid.New()
	if _, err := u.Execute(context.Background(), a, ListRoutinesInput{Query: "pierna", Cursor: *page.NextCursor}); !errors.Is(err, protocol.ErrCursorEpochChanged) {
		t.Fatal("epoch accepted", err)
	}
	stream.epoch = uuid.New()
	rows.rows = nil
	empty, err := u.Execute(context.Background(), a, ListRoutinesInput{Query: strings.Repeat("한", 120)})
	if err != nil || !reflect.DeepEqual(empty.Items, []RoutineDTO{}) {
		t.Fatal("long Unicode query or empty array", err)
	}
}
func TestRoutineListRejectsBrokenRepositoryOrderingAndHiddenRows(t *testing.T) {
	a, uow, commands, repo, now := routineReaderFixture(t)
	signer, _ := cursorSecurity.NewCursorSigner("test", map[string][]byte{"test": []byte(strings.Repeat("s", 32))})
	rows := &routineReadList{}
	u, _ := NewListRoutines(ListRoutinesDependencies{UoW: uow, Commands: commands, Stream: &routineReadStream{epoch: uuid.New()}, Routines: rows, Cursors: signer, Now: func() time.Time { return now }})
	for _, change := range []func(*routine.Routine){func(r *routine.Routine) { r.AccountID = uuid.New() }, func(r *routine.Routine) { r.DeletedAt = &now }, func(r *routine.Routine) { r.Status = "archived" }, func(r *routine.Routine) { r.Title = "otro" }} {
		bad := *repo.root
		change(&bad)
		rows.rows = []*routine.Routine{&bad}
		if out, err := u.Execute(context.Background(), a, ListRoutinesInput{Query: "pierna"}); !errors.Is(err, trainingErrors.ErrTrainingPersistence) || out.Items != nil {
			t.Fatal("invalid projection escaped", err)
		}
	}
	rows.rows = []*routine.Routine{repo.root, repo.root}
	if _, err := u.Execute(context.Background(), a, ListRoutinesInput{}); !errors.Is(err, trainingErrors.ErrTrainingPersistence) {
		t.Fatal("duplicate ordering accepted", err)
	}
}
