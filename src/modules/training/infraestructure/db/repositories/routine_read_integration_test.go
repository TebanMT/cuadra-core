//go:build server && integration

package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	commandRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	cursorSecurity "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/google/uuid"
)

func routineListUseCase(t *testing.T, base trainingApp.CreatePersonalExerciseDependencies) *trainingApp.ListRoutines {
	t.Helper()
	cursors, err := cursorSecurity.NewCursorSigner("test", map[string][]byte{"test": []byte(strings.Repeat("r", 32))})
	if err != nil {
		t.Fatal(err)
	}
	u, err := trainingApp.NewListRoutines(trainingApp.ListRoutinesDependencies{UoW: base.UoW, Commands: base.Commands, Stream: commandRepos.NewStream(), Routines: NewRoutinePostgresRepository(), Cursors: cursors, Now: base.Now})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func routineReadDependencies(base trainingApp.CreatePersonalExerciseDependencies) trainingApp.RoutineReadDependencies {
	return trainingApp.RoutineReadDependencies{UoW: base.UoW, Commands: base.Commands, Routines: NewRoutinePostgresRepository(), Publications: NewRoutinePublicationPostgresRepository()}
}

func TestRoutineReadPostgresSearchAndKeysetBoundaries(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, bbase := trainingFixture(t)
	create := func(actor security.AccessClaims, deps trainingApp.CreatePersonalExerciseDependencies, title string) string {
		c := routineCommand(actor)
		c.Payload.Draft.Title = title
		createDraftFixture(t, deps, actor, c)
		return c.EntityID
	}
	first := create(a, base, "  PIÉRNA\u00a0  Completa ")
	second := create(a, base, "Pie\u0301rna\tcompleta")
	literal := create(a, base, "100%_ literal")
	archived := create(a, base, "PIERNA completa")
	deleted := create(a, base, "PIERNA completa")
	create(b, bbase, "PIERNA completa")
	if err := owner.Exec("UPDATE mancuerna.routines SET status='archived' WHERE id=?", archived).Error; err != nil {
		t.Fatal(err)
	}
	if err := owner.Exec("UPDATE mancuerna.routines SET deleted_at=clock_timestamp() WHERE id=?", deleted).Error; err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for id, offset := range map[string]time.Duration{first: time.Microsecond, second: 0} {
		if err := owner.Exec("UPDATE mancuerna.routines SET created_at=? WHERE id=?", at.Add(offset), id).Error; err != nil {
			t.Fatal(err)
		}
	}
	u := routineListUseCase(t, base)
	page, err := u.Execute(context.Background(), a, trainingApp.ListRoutinesInput{Query: " piérna\n completa ", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != first || page.NextCursor == nil {
		t.Fatal("normalized first page", err)
	}
	next, err := u.Execute(context.Background(), a, trainingApp.ListRoutinesInput{Status: "active", Query: "PIERNA COMPLETA", Cursor: *page.NextCursor, Limit: 100})
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != second || next.NextCursor != nil {
		t.Fatal("stable microsecond page", err)
	}
	for _, query := range []string{"%_", "100%_", "_ literal"} {
		got, err := u.Execute(context.Background(), a, trainingApp.ListRoutinesInput{Query: query})
		if err != nil || len(got.Items) != 1 || got.Items[0].ID != literal {
			t.Fatal("literal wildcard search", err)
		}
	}
	for _, status := range []string{"archived", "all"} {
		got, err := u.Execute(context.Background(), a, trainingApp.ListRoutinesInput{Query: "PIERNA COMPLETA", Status: status})
		want := 1
		if status == "all" {
			want = 3
		}
		if err != nil || len(got.Items) != want {
			t.Fatal("status or deleted isolation", status, err)
		}
	}
	for _, in := range []trainingApp.ListRoutinesInput{{Cursor: *page.NextCursor, Query: "otro"}, {Cursor: *page.NextCursor, Query: "pierna completa", Status: "all"}} {
		if out, err := u.Execute(context.Background(), a, in); !errors.Is(err, protocol.ErrCursorInvalid) || out.Items != nil {
			t.Fatal("cursor filter accepted", err)
		}
	}
	if _, err := u.Execute(context.Background(), b, trainingApp.ListRoutinesInput{Query: "pierna completa", Cursor: *page.NextCursor}); !errors.Is(err, protocol.ErrCursorInvalid) {
		t.Fatal("cursor crosses owner", err)
	}
	if err := owner.Exec("UPDATE mancuerna.sync_streams SET epoch=? WHERE account_id=?", uuid.New(), a.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(context.Background(), a, trainingApp.ListRoutinesInput{Query: "pierna completa", Cursor: *page.NextCursor}); !errors.Is(err, protocol.ErrCursorEpochChanged) {
		t.Fatal("cursor ignores epoch", err)
	}
	// Identical Unicode transforms in SQL and Go, including decomposed marks,
	// non-ASCII whitespace, Hangul and letters whose lowercase is locale-sensitive.
	for _, vector := range []struct{ raw, want string }{
		{"  PIÉRNA\u00a0  Completa ", "pierna completa"}, {"Pie\u0301rna\tcompleta", "pierna completa"}, {"\u2003ÍSTANBUL\n漢 한", "istanbul 漢 한"}, {"ΣΙΓΜΑ İ I", "σιγμα i i"}, {"a\u1ab0b\u0301", "ab"}, {"100%_\\ literal", "100%_\\ literal"}, {"\t\n", ""},
		{"Straße STRASSE STRAẞE", "strasse strasse strasse"}, {"ΟΣ ος οσ Σ ς σ", "οσ οσ οσ σ σ σ"}, {"ﬀ ﬁ ﬂ ﬃ ﬄ ﬅ ﬆ", "ff fi fl ffi ffl st st"}, {"և ԵՒ", "եւ եւ"}, {"Ꭰ ꭰ", "ꭰ ꭰ"},
		{strings.Repeat("ß", 120), strings.Repeat("ss", 120)}, {strings.Repeat("ﬃ", 120), strings.Repeat("ffi", 120)}, {strings.Repeat("한", 120), strings.Repeat("한", 120)},
	} {
		var got string
		expression, args := routineSearchExpression(&vector.raw)
		if err := owner.Raw("SELECT "+expression, args...).Row().Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != vector.want || routine.NormalizeSearch(vector.raw) != vector.want || routine.NormalizeSearch(got) != got {
			t.Fatalf("SQL/Go search mismatch for %q", vector.raw)
		}
	}
}

func TestRoutineReadPostgresUnicodeFoldCursorContinuity(t *testing.T) {
	owner, actor, base := trainingFixture(t)
	u := routineListUseCase(t, base)
	for _, vector := range []struct {
		titles      [2]string
		first, next string
	}{
		{[2]string{"Straße Plan", "STRASSE Plan"}, "STRASSE", "straße"},
		{[2]string{"ΟΣ", "ος"}, "ΟΣ", "ος"},
		{[2]string{"ﬃ Plan", "FFI Plan"}, "ﬃ", "FFI"},
		{[2]string{"Ꭰ Plan", "ꭰ Plan"}, "Ꭰ", "ꭰ"},
	} {
		var ids [2]string
		at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		for i, title := range vector.titles {
			c := routineCommand(actor)
			c.Payload.Draft.Title = title
			createDraftFixture(t, base, actor, c)
			ids[i] = c.EntityID
			if err := owner.Exec("UPDATE mancuerna.routines SET created_at=? WHERE id=?", at.Add(time.Duration(1-i)*time.Microsecond), c.EntityID).Error; err != nil {
				t.Fatal(err)
			}
		}
		page, err := u.Execute(context.Background(), actor, trainingApp.ListRoutinesInput{Query: vector.first, Limit: 1})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != ids[0] || page.NextCursor == nil {
			t.Fatal("folded first page", err)
		}
		next, err := u.Execute(context.Background(), actor, trainingApp.ListRoutinesInput{Query: vector.next, Limit: 1, Cursor: *page.NextCursor})
		if err != nil || len(next.Items) != 1 || next.Items[0].ID != ids[1] || next.NextCursor != nil {
			t.Fatal("equivalent folded query changed cursor binding", err)
		}
	}
	for _, title := range []string{strings.Repeat("ß", 120), strings.Repeat("ﬃ", 120), strings.Repeat("한", 120)} {
		c := routineCommand(actor)
		c.Payload.Draft.Title = title
		createDraftFixture(t, base, actor, c)
		page, err := u.Execute(context.Background(), actor, trainingApp.ListRoutinesInput{Query: title})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != c.EntityID {
			t.Fatal("fold expansion exceeded internal filter bound", err)
		}
	}
}

func TestRoutineReadPostgresDraftTombstoneVersionAndRevocation(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	c := completeRoutine(t, base, a)
	p := publicationCommand(a, c, 1, 1)
	requirePublicationOutcome(t, publicationUseCase(t, publicationDependencies(base)), a, p, "accepted", "")
	d := routineReadDependencies(base)
	get, _ := trainingApp.NewGetRoutine(d)
	draft, _ := trainingApp.NewGetRoutineDraft(d)
	version, _ := trainingApp.NewGetRoutineVersion(d)
	list := routineListUseCase(t, base)
	id, vid := uuid.MustParse(c.EntityID), uuid.MustParse(p.Payload.VersionID)
	root, err := get.Execute(context.Background(), a, id)
	if err != nil || root.Status != "ready" || root.Revision != "2" || root.CurrentVersionID == nil || *root.CurrentVersionID != vid.String() {
		t.Fatal("published root", err)
	}
	edit, err := draft.Execute(context.Background(), a, id)
	if err != nil || edit.Content != nil || edit.DeletedAt == nil || edit.Revision != "2" || edit.BaseVersionID == nil || *edit.BaseVersionID != vid.String() {
		t.Fatal("consumed draft not returned", err)
	}
	v, err := version.Execute(context.Background(), a, vid)
	if err != nil || len(v.Days) != 2 || len(v.Days[0].Exercises) != 2 {
		t.Fatal("complete version", err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = protocol.DecodeRoutineVersion(raw, base.Canonicalizer); err != nil {
		t.Fatal("sealed hash", err)
	}
	for _, foreign := range []uuid.UUID{uuid.New(), id} {
		if _, err := version.Execute(context.Background(), a, foreign); !errors.Is(err, trainingErrors.ErrRoutineNotFound) {
			t.Fatal("root/missing ID treated as version", err)
		}
	}
	if _, err := get.Execute(context.Background(), b, id); !errors.Is(err, trainingErrors.ErrRoutineNotFound) {
		t.Fatal("foreign root", err)
	}
	if _, err := draft.Execute(context.Background(), b, id); !errors.Is(err, trainingErrors.ErrRoutineNotFound) {
		t.Fatal("foreign draft", err)
	}
	if _, err := version.Execute(context.Background(), b, vid); !errors.Is(err, trainingErrors.ErrRoutineNotFound) {
		t.Fatal("foreign version", err)
	}
	if err := owner.Exec("UPDATE mancuerna.routines SET revision=9007199254740993,deleted_at=clock_timestamp() WHERE id=?", id).Error; err != nil {
		t.Fatal(err)
	}
	root, err = get.Execute(context.Background(), a, id)
	if err != nil || root.Revision != "9007199254740993" || root.DeletedAt == nil {
		t.Fatal("root tombstone / bigint", err)
	}
	if got, err := version.Execute(context.Background(), a, vid); err != nil || !reflect.DeepEqual(got, v) {
		t.Fatal("root deletion altered historical snapshot", err)
	}
	sequence := trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID)
	if err := owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", a.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func() error{"root": func() error { _, err := get.Execute(context.Background(), a, id); return err }, "draft": func() error { _, err := draft.Execute(context.Background(), a, id); return err }, "version": func() error { _, err := version.Execute(context.Background(), a, vid); return err }, "list": func() error {
		_, err := list.Execute(context.Background(), a, trainingApp.ListRoutinesInput{})
		return err
	}} {
		if err := read(); !errors.Is(err, protocol.ErrCommandActor) {
			t.Fatal("revocation ignored", name, err)
		}
	}
	if trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != sequence {
		t.Fatal("reads advanced journal")
	}
}
