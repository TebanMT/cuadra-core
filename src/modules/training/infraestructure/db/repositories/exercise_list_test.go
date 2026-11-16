//go:build server

package repositories

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/exercise"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

func TestExerciseListSQLBindsNamesQueryAndSource(t *testing.T) {
	owner := uuid.New()
	query := "x%_'\\) union select private"
	after := &exercise.ListPosition{Name: "name ' ); --", Source: exercise.CatalogSource, ID: uuid.New()}
	sql, args := exerciseListStatement(owner, exercise.ListFilter{Query: query}, after, 2)
	if strings.Contains(sql, query) || strings.Contains(sql, after.Name) || strings.Contains(sql, owner.String()) || strings.Contains(sql, after.ID.String()) {
		t.Fatal("user material interpolated into SQL")
	}
	for _, fragment := range []string{"UNION ALL", "account_id=?", "position(? IN", `name COLLATE "C"`, `source COLLATE "C"`, "id ASC LIMIT ?"} {
		if !strings.Contains(sql, fragment) {
			t.Fatal("query lost source identity/filter/order", fragment)
		}
	}
	if args[0] != owner || args[1] != query || !reflect.DeepEqual(args[len(args)-4:], []any{after.Name, after.Source, after.ID, 2}) {
		t.Fatal("bound query arguments changed")
	}
	if strings.Count(sql, "?") != len(args) {
		t.Fatal("placeholder count", len(args))
	}
	nameSQL, nameArgs := trainingSearchExpression(searchName, nil)
	titleSQL, titleArgs := routineSearchExpression(nil)
	if strings.Replace(titleSQL, "normalize(title,NFD)", "normalize(name,NFD)", 1) != nameSQL || !reflect.DeepEqual(nameArgs, titleArgs) {
		t.Fatal("exercise and routine search semantics differ")
	}
}

func TestExerciseListRepositoryRejectsUnnormalizedInputAndMissingScope(t *testing.T) {
	r := NewExerciseListPostgresRepository()
	owner := uuid.New()
	for _, query := range []string{"Straße", "x\x00", "\xff", strings.Repeat("x", 4097)} {
		if rows, err := r.List(nil, owner, exercise.ListFilter{Query: query}, nil, 2); !errors.Is(err, trainingErrors.ErrInvalidExercise) || rows != nil {
			t.Fatal("invalid filter accepted", err)
		}
	}
	if _, err := r.List(nil, owner, exercise.ListFilter{}, nil, 2); !errors.Is(err, protocol.ErrCommandScope) {
		t.Fatal("missing transaction accepted", err)
	}
}
