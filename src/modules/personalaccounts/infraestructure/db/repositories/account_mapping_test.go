//go:build server

package repositories

import (
	"reflect"
	"testing"
	"time"

	accountDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	"github.com/google/uuid"
)

func TestProfileMappingPreservesFieldsNormalizesUTCAndCopiesPointers(t *testing.T) {
	local := time.Date(2026, 10, 19, 1, 2, 3, 456000000, time.FixedZone("test", -6*3600))
	a := &accountDomain.Account{ID: uuid.New(), DisplayName: "Ana", WeightUnit: accountDomain.Pounds, Revision: 9007199254740993, Status: accountDomain.Deleted, CreatedAt: local, UpdatedAt: local, DeletedAt: &local}
	row := toModel(a)
	if row.CreatedAt.Location() != time.UTC || row.UpdatedAt.Location() != time.UTC || row.DeletedAt.Location() != time.UTC {
		t.Fatal("storage timestamps not UTC")
	}
	if row.DeletedAt == a.DeletedAt {
		t.Fatal("mapper aliased aggregate deletion pointer")
	}
	got, err := toDomain(&row)
	if err != nil {
		t.Fatal(err)
	}
	expected := *a
	expected.CreatedAt = local.UTC()
	expected.UpdatedAt = local.UTC()
	utc := local.UTC()
	expected.DeletedAt = &utc
	if !reflect.DeepEqual(got, &expected) {
		t.Fatalf("mapping lost profile state: %#v", got)
	}
	if got.DeletedAt == row.DeletedAt {
		t.Fatal("mapper aliased database deletion pointer")
	}
}
