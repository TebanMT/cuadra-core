package exercise

import (
	"errors"
	"strings"
	"testing"
	"time"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

func TestExerciseListUnicodeFilterAndOriginalCursorKey(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{"  PIÉRNA\u2003Completa\n", "pierna completa"}, {"Pie\u0301rna\tcompleta", "pierna completa"},
		{"Straße STRASSE", "strasse strasse"}, {"ΟΣ ος", "οσ οσ"}, {"Ꭰ ꭰ", "ꭰ ꭰ"},
		{"%_\\'", "%_\\'"}, {strings.Repeat("ﬃ", 120), strings.Repeat("ffi", 120)}, {"", ""},
	} {
		got, err := NormalizeListFilter(test.raw)
		if err != nil || got.Query != test.want {
			t.Fatalf("filter %q: %q %v", test.raw, got.Query, err)
		}
	}
	for _, bad := range []string{strings.Repeat("😀", 121), "\xff", "x\x00"} {
		if _, err := NormalizeListFilter(bad); !errors.Is(err, trainingErrors.ErrInvalidExercise) {
			t.Fatal("invalid query", err)
		}
	}
	for _, name := range []string{"catalog:personal:\nß", strings.Repeat("😀", 120)} {
		original := ListPosition{Name: name, Source: PersonalSource, ID: uuid.New()}
		cursor := original.CursorPosition()
		got, err := PositionFromCursor(cursor)
		if err != nil || *got != original || len(cursor.Key) > 512 {
			t.Fatal("original cursor key lost or oversized", err)
		}
	}
	for _, bad := range []protocol.CursorPosition{{ID: uuid.New(), Key: "unknown:name"}, {ID: uuid.New(), Key: "catalog:"}, {ID: uuid.New(), Key: "catalog: "}, {ID: uuid.New(), Key: "personal:name", Sequence: "1"}, {ID: uuid.Nil, Key: "catalog:name"}, {ID: uuid.New(), Key: "catalog:" + strings.Repeat("a", 121)}} {
		if _, err := PositionFromCursor(bad); !errors.Is(err, protocol.ErrCursorInvalid) {
			t.Fatal("invalid position", err)
		}
	}
}

func TestExerciseListSourceIdentityAndOwnership(t *testing.T) {
	now := time.Now().UTC()
	e := &ListedExercise{Exercise: Exercise{ID: uuid.New(), Name: "Sentadilla", LoadMode: None, Revision: 1, CreatedAt: now, UpdatedAt: now}, Source: CatalogSource}
	if err := e.Validate(); err != nil {
		t.Fatal("catalog requires no invented account", err)
	}
	p := *e
	p.Source, p.AccountID = PersonalSource, uuid.New()
	if p.Validate() != nil || !p.Position().After(e.Position()) || e.Position().After(p.Position()) {
		t.Fatal("same UUID/name from two sources conflated")
	}
	for _, change := range []func(*ListedExercise){
		func(v *ListedExercise) { v.AccountID = uuid.New() },
		func(v *ListedExercise) { v.Source = PersonalSource },
		func(v *ListedExercise) { v.Source = "other" },
		func(v *ListedExercise) { v.UpdatedAt = now.Add(-time.Second) },
	} {
		bad := *e
		change(&bad)
		if !errors.Is(bad.Validate(), trainingErrors.ErrInvalidExercise) {
			t.Fatal("invalid source projection accepted")
		}
	}
}
