package workout_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
)

func TestWeightSharedGolden(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../../../../contracts/mancuerna/v1/fixtures/workout-weights.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version int `json:"version"`
		Valid   []struct {
			Input, Canonical string
		}
		Invalid []string
	}
	if err = json.Unmarshal(raw, &fixture); err != nil || fixture.Version != 1 || len(fixture.Valid) != 18 || len(fixture.Invalid) != 32 {
		t.Fatalf("invalid shared weight fixture: %v", err)
	}
	for i, vector := range fixture.Valid {
		t.Run(fmt.Sprintf("valid-%d", i), func(t *testing.T) {
			got, err := workout.CanonicalWeight(vector.Input)
			if err != nil || got != vector.Canonical {
				t.Fatalf("want %q got %q: %v", vector.Canonical, got, err)
			}
			if second, err := workout.CanonicalWeight(got); err != nil || second != got {
				t.Fatalf("canonical value not stable: %q %v", second, err)
			}
			for _, unit := range []string{"kg", "lb"} {
				got, err := workout.CanonicalLoad("external", &vector.Input, &unit)
				if err != nil || got.LoadMode != "external" || got.WeightValue == nil || *got.WeightValue != vector.Canonical || got.WeightUnit == nil || *got.WeightUnit != unit {
					t.Fatalf("load changed magnitude/unit: %+v %v", got, err)
				}
			}
		})
	}
	for i, input := range fixture.Invalid {
		t.Run(fmt.Sprintf("invalid-%d", i), func(t *testing.T) {
			got, err := workout.CanonicalWeight(input)
			if got != "" || !errors.Is(err, trainingErrors.ErrInvalidWeight) {
				t.Fatalf("invalid grammar accepted or rounded: %q %v", got, err)
			}
		})
	}
}

func TestWeightLoadNullsAndNoInputAliasing(t *testing.T) {
	got, err := workout.CanonicalLoad("none", nil, nil)
	if err != nil || got.LoadMode != "none" || got.WeightValue != nil || got.WeightUnit != nil {
		t.Fatalf("none must retain nulls: %+v %v", got, err)
	}
	value, unit := "30.0", "lb"
	got, err = workout.CanonicalLoad("external", &value, &unit)
	if err != nil || value != "30.0" || unit != "lb" || got.WeightValue == &value || got.WeightUnit == &unit {
		t.Fatalf("caller input changed or aliased: %+v %v", got, err)
	}
	value, unit = "7.125", "kg"
	if *got.WeightValue != "30.000" || *got.WeightUnit != "lb" {
		t.Fatal("canonical result retained input pointers")
	}
	for _, input := range []struct {
		mode        string
		value, unit *string
	}{
		{"none", &value, nil}, {"none", nil, &unit}, {"none", &value, &unit},
		{"external", nil, nil}, {"external", &value, nil}, {"external", nil, &unit},
		{"External", &value, &unit}, {"", nil, nil}, {"unknown", &value, &unit},
	} {
		bad, err := workout.CanonicalLoad(input.mode, input.value, input.unit)
		if bad != (workout.Load{}) || !errors.Is(err, trainingErrors.ErrInvalidWeight) {
			t.Fatalf("invalid mode/null combination returned a load: %+v %v", bad, err)
		}
	}
	for _, unit := range []string{"KG", "lbs", "", " kg", "lb "} {
		bad, err := workout.CanonicalLoad("external", &value, &unit)
		if bad != (workout.Load{}) || !errors.Is(err, trainingErrors.ErrInvalidWeight) {
			t.Fatal("invalid unit accepted")
		}
	}
}
