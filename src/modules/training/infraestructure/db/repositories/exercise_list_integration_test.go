//go:build server && integration

package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	trainingApp "github.com/cuadra/cuadra-core/src/modules/training/app"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	commandRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	cursorSecurity "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func exerciseListUseCase(t *testing.T, base trainingApp.CreatePersonalExerciseDependencies) *trainingApp.ListExercises {
	t.Helper()
	cursors, err := cursorSecurity.NewCursorSigner("test", map[string][]byte{"test": []byte(strings.Repeat("e", 32))})
	if err != nil {
		t.Fatal(err)
	}
	u, err := trainingApp.NewListExercises(trainingApp.ListExercisesDependencies{UoW: base.UoW, Commands: base.Commands, Stream: commandRepos.NewStream(), Exercises: NewExerciseListPostgresRepository(), Cursors: cursors, Now: base.Now})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func listedPersonalFixture(t *testing.T, base trainingApp.CreatePersonalExerciseDependencies, actor security.AccessClaims, name string) string {
	t.Helper()
	c := trainingCommand(actor)
	c.Payload.Name = name
	if out, err := trainingUseCase(t, base).Execute(context.Background(), actor, c); err != nil || out.Outcome != "accepted" {
		t.Fatal("personal exercise setup", err)
	}
	return c.EntityID
}
func listedCatalogFixture(t *testing.T, owner *gorm.DB, id, name string) {
	t.Helper()
	// Synthetic test fixture, not an editorial seed. An intentionally stale
	// search_key proves read behavior is independent of legacy derived values.
	if err := owner.Exec("INSERT INTO mancuerna.catalog_exercises(id,name,search_key,load_mode,notes,revision) VALUES(?,?,'legacy','none','Catálogo sintético',9007199254740993)", id, name).Error; err != nil {
		t.Fatal("catalog setup", err)
	}
}

func TestExerciseListPostgresUnionIdentityActiveScopeAndCursor(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, bbase := trainingFixture(t)
	marker := uuid.NewString()
	name := marker + " PIÉRNA"
	one := listedPersonalFixture(t, base, a, name)
	two := listedPersonalFixture(t, base, a, name)
	listedCatalogFixture(t, owner, one, name) // The same ID and name in both sources is legal.
	foreign := listedPersonalFixture(t, bbase, b, name)
	archivedPersonal := listedPersonalFixture(t, base, a, name)
	deletedPersonal := listedPersonalFixture(t, base, a, name)
	archivedCatalog, deletedCatalog := uuid.NewString(), uuid.NewString()
	listedCatalogFixture(t, owner, archivedCatalog, name)
	listedCatalogFixture(t, owner, deletedCatalog, name)
	for _, statement := range []struct {
		sql string
		id  string
	}{
		{"UPDATE mancuerna.personal_exercises SET archived_at=clock_timestamp() WHERE id=?", archivedPersonal},
		{"UPDATE mancuerna.personal_exercises SET deleted_at=clock_timestamp() WHERE id=?", deletedPersonal},
		{"UPDATE mancuerna.catalog_exercises SET archived_at=clock_timestamp() WHERE id=?", archivedCatalog},
		{"UPDATE mancuerna.catalog_exercises SET deleted_at=clock_timestamp() WHERE id=?", deletedCatalog},
		{"UPDATE mancuerna.personal_exercises SET revision=9007199254740993 WHERE id=?", one},
	} {
		if err := owner.Exec(statement.sql, statement.id).Error; err != nil {
			t.Fatal(err)
		}
	}
	sequence := trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID)
	u := exerciseListUseCase(t, base)
	var cursor string
	var firstCursor string
	var got []string
	for pageIndex := 0; pageIndex < 4; pageIndex++ {
		query := " \t" + marker + " piérna\n"
		if pageIndex > 0 {
			query = marker + " PIERNA"
		}
		page, err := u.Execute(context.Background(), a, trainingApp.ListExercisesInput{Query: query, Cursor: cursor, Limit: 1})
		if err != nil || len(page.Items) != 1 {
			t.Fatal("paged active union", err)
		}
		item := page.Items[0]
		got = append(got, item.Source+":"+item.ID)
		if item.Name != name || item.ArchivedAt != nil || item.DeletedAt != nil {
			t.Fatal("display/active projection")
		}
		if item.ID == one && item.Revision != "9007199254740993" {
			t.Fatal("bigint lost precision")
		}
		if item.Source == "catalog" && (item.Notes != "Catálogo sintético" || item.LoadMode != "none") {
			t.Fatal("catalog mapped as personal")
		}
		if item.Source == "personal" && (item.Notes != "Nota\nsegunda" || item.LoadMode != "external") {
			t.Fatal("personal mapped as catalog")
		}
		encoded, _ := json.Marshal(item)
		var wire map[string]any
		_ = json.Unmarshal(encoded, &wire)
		if len(wire) != 10 {
			t.Fatal("unexpected public exercise fields")
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
		if firstCursor == "" {
			firstCursor = cursor
		}
	}
	personalIDs := []string{one, two}
	sort.Strings(personalIDs)
	want := []string{"catalog:" + one, "personal:" + personalIDs[0], "personal:" + personalIDs[1]}
	if !reflect.DeepEqual(got, want) || firstCursor == "" {
		t.Fatal("source identity or equal-name keyset lost", got, want)
	}
	other, err := u.Execute(context.Background(), b, trainingApp.ListExercisesInput{Query: marker})
	if err != nil || len(other.Items) != 2 || other.Items[0].Source != "catalog" || other.Items[1].ID != foreign {
		t.Fatal("catalog visibility or personal isolation", err)
	}
	for _, in := range []trainingApp.ListExercisesInput{{Query: "other", Cursor: firstCursor}, {Query: marker + " PIERNA", Cursor: firstCursor + "x"}} {
		if out, err := u.Execute(context.Background(), a, in); !errors.Is(err, protocol.ErrCursorInvalid) || out.Items != nil {
			t.Fatal("changed filter/token", err)
		}
	}
	if _, err := u.Execute(context.Background(), b, trainingApp.ListExercisesInput{Query: marker + " PIERNA", Cursor: firstCursor}); !errors.Is(err, protocol.ErrCursorInvalid) {
		t.Fatal("foreign cursor", err)
	}
	if err := owner.Exec("UPDATE mancuerna.sync_streams SET epoch=? WHERE account_id=?", uuid.New(), a.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(context.Background(), a, trainingApp.ListExercisesInput{Query: marker + " PIERNA", Cursor: firstCursor}); !errors.Is(err, protocol.ErrCursorEpochChanged) {
		t.Fatal("epoch cursor", err)
	}
	if trainingCount(t, owner, "SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", a.AccountID) != sequence {
		t.Fatal("list advanced personal journal")
	}
}

func TestExerciseListPostgresUnicodeLiteralSearchAndKeysetBoundaries(t *testing.T) {
	owner, a, base := trainingFixture(t)
	u := exerciseListUseCase(t, base)
	for _, vector := range []struct {
		names       [2]string
		first, next string
	}{
		{[2]string{"  PIÉRNA\u00a0 completa ", "Pie\u0301rna\tCOMPLETA"}, "piérna completa", "PIERNA\n completa"},
		{[2]string{"Straße", "STRASSE"}, "STRASSE", "straße"},
		{[2]string{"ΟΣ", "ος"}, "ΟΣ", "ος"},
		{[2]string{"ﬃ", "FFI"}, "ﬃ", "ffi"},
		{[2]string{"Ꭰ", "ꭰ"}, "Ꭰ", "ꭰ"},
	} {
		marker := uuid.NewString()
		var items []trainingApp.ExerciseDTO
		for i, name := range vector.names {
			fullName := marker + " " + name
			id := uuid.NewString()
			source := "catalog"
			if i == 0 {
				listedCatalogFixture(t, owner, id, fullName)
			} else {
				id = listedPersonalFixture(t, base, a, fullName)
				source = "personal"
			}
			items = append(items, trainingApp.ExerciseDTO{ID: id, Source: source, Name: fullName})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		page, err := u.Execute(context.Background(), a, trainingApp.ListExercisesInput{Query: marker + " " + vector.first, Limit: 1})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != items[0].ID || page.NextCursor == nil {
			t.Fatal("Unicode first page", vector.first, err)
		}
		next, err := u.Execute(context.Background(), a, trainingApp.ListExercisesInput{Query: marker + " " + vector.next, Cursor: *page.NextCursor, Limit: 1})
		if err != nil || len(next.Items) != 1 || next.Items[0].ID != items[1].ID || next.NextCursor != nil {
			t.Fatal("Unicode continuation", vector.next, err)
		}
	}
	marker := uuid.NewString()
	literalName := marker + " %_'\\ literal"
	literal := listedPersonalFixture(t, base, a, literalName)
	listedCatalogFixture(t, owner, uuid.NewString(), marker+" xx literal")
	for _, query := range []string{marker + " %_", marker + " %_'\\"} {
		got, err := u.Execute(context.Background(), a, trainingApp.ListExercisesInput{Query: query})
		if err != nil || len(got.Items) != 1 || got.Items[0].ID != literal {
			t.Fatal("wildcard/quote was not literal", err)
		}
	}
	for _, name := range []string{strings.Repeat("ß", 120), strings.Repeat("ﬃ", 120), strings.Repeat("한", 120), strings.Repeat("😀", 120)} {
		one := listedPersonalFixture(t, base, a, name)
		two := listedPersonalFixture(t, base, a, name)
		ids := []string{one, two}
		sort.Strings(ids)
		page, err := u.Execute(context.Background(), a, trainingApp.ListExercisesInput{Query: name, Limit: 1})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != ids[0] || page.NextCursor == nil {
			t.Fatal("expanded query/long cursor first page", err)
		}
		next, err := u.Execute(context.Background(), a, trainingApp.ListExercisesInput{Query: name, Cursor: *page.NextCursor, Limit: 1})
		if err != nil || len(next.Items) != 1 || next.Items[0].ID != ids[1] || next.NextCursor != nil {
			t.Fatal("expanded query/long cursor continuation", err)
		}
	}
}

func TestExerciseListPostgresCatalogRequiresActiveActorAndDataRole(t *testing.T) {
	owner, a, base := trainingFixture(t)
	_, b, _ := trainingFixture(t)
	marker := uuid.NewString()
	listedCatalogFixture(t, owner, uuid.NewString(), marker)
	u := exerciseListUseCase(t, base)
	if page, err := u.Execute(context.Background(), a, trainingApp.ListExercisesInput{Query: marker}); err != nil || len(page.Items) != 1 || page.Items[0].Source != "catalog" {
		t.Fatal("active catalog read", err)
	}
	mixed := a
	mixed.AccountID = b.AccountID
	if out, err := u.Execute(context.Background(), mixed, trainingApp.ListExercisesInput{Query: marker}); !errors.Is(err, protocol.ErrCommandActor) || out.Items != nil {
		t.Fatal("mixed actor read catalog", err)
	}
	auth, err := shared.NewPersonalPostgresUnitOfWork(trainingTestPool(t, "MANCUERNA_TEST_AUTH_DSN"), shared.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewExerciseListPostgresRepository()
	if err := auth.Command(shared.WithPersonalAccount(context.Background(), a.AccountID), func(tx shared.Transaction) error {
		_, err := repo.List(tx, a.AccountID, exercise.ListFilter{}, nil, 2)
		return err
	}); !errors.Is(err, protocol.ErrCommandScope) {
		t.Fatal("auth role read data repository", err)
	}
	query, err := base.UoW.Query(shared.WithPersonalAccount(context.Background(), a.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.List(query, b.AccountID, exercise.ListFilter{}, nil, 2); !errors.Is(err, protocol.ErrCommandScope) {
		t.Fatal("explicit foreign account", err)
	}
	shared.CloseQuery(query)
	if err := owner.Exec("UPDATE mancuerna.sessions SET revoked_at=clock_timestamp() WHERE id=?", a.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if out, err := u.Execute(context.Background(), a, trainingApp.ListExercisesInput{Query: marker}); !errors.Is(err, protocol.ErrCommandActor) || out.Items != nil {
		t.Fatal("revoked actor read public catalog", err)
	}
	trainingAssertEmpty(t, owner, a.AccountID)
}
