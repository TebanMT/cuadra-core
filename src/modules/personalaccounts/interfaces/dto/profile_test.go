package dto

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	accountDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/google/uuid"
)

func TestProfileBoundaryPreservesLargeRevisionAndOnlyContractFields(t *testing.T) {
	a, err := accountDomain.NewAccount(uuid.New(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	a.Revision = 9007199254740993
	profile, err := ProfileFromDomain(a)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Revision != "9007199254740993" || profile.ID != a.ID.String() {
		t.Fatal("identity/revision lost precision", profile)
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	if len(object) != 4 {
		t.Fatal("profile exposed fields outside S00", object)
	}
	for _, key := range []string{"id", "display_name", "weight_unit", "revision"} {
		if _, ok := object[key].(string); !ok {
			t.Fatal("non-string boundary field", key)
		}
	}
	encoded, err = json.Marshal(ProfileUpdate{DisplayName: "Ana", WeightUnit: "kg"})
	if err != nil {
		t.Fatal(err)
	}
	object = nil
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	if len(object) != 2 || object["display_name"] != "Ana" || object["weight_unit"] != "kg" {
		t.Fatal("profile payload exposed extra mutation fields", object)
	}
}

func TestProfileRevisionBoundaryRejectsAmbiguityAndOverflow(t *testing.T) {
	for _, input := range []string{"", "0", "-1", "+1", "01", "1.0", "1e3", " 1", "1 ", "9223372036854775808"} {
		if _, err := ParseProfileRevision(input); !errors.Is(err, personalErrors.ErrInvalidProfileRevision) {
			t.Fatal("invalid revision accepted", input, err)
		}
	}
	for input, want := range map[string]int64{"1": 1, "9007199254740993": 9007199254740993, "9223372036854775807": math.MaxInt64} {
		got, err := ParseProfileRevision(input)
		if err != nil || got != want {
			t.Fatal("valid revision rejected/rounded", input, got, err)
		}
	}
}
