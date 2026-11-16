package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
)

type routineGolden struct {
	Version int `json:"version"`
	Valid   []struct {
		Name             string          `json:"name"`
		DTO              json.RawMessage `json:"dto"`
		CanonicalContent string          `json:"canonical_content"`
		Hash             string          `json:"hash"`
	} `json:"valid"`
	Invalid []struct {
		Name  string          `json:"name"`
		DTO   json.RawMessage `json:"dto"`
		Error string          `json:"error"`
	} `json:"invalid"`
}

func routineVectors(t *testing.T) routineGolden {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "../../../../../contracts/mancuerna/v1/fixtures/routine-version-hashes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture routineGolden
	if err = json.Unmarshal(raw, &fixture); err != nil || fixture.Version != 1 || len(fixture.Valid) != 7 || len(fixture.Invalid) != 25 {
		t.Fatalf("invalid shared fixture: %v", err)
	}
	return fixture
}
func routineMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
func routineJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func routineCanonical(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := (crypto.JSONCanonicalizer{}).Canonicalize(routineJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRoutineVersionSharedGolden(t *testing.T) {
	fixture := routineVectors(t)
	hashes := map[string]bool{}
	for _, vector := range fixture.Valid {
		t.Run(vector.Name, func(t *testing.T) {
			verified, err := protocol.DecodeRoutineVersion(vector.DTO, crypto.JSONCanonicalizer{})
			if err != nil || verified.ContentHash != vector.Hash {
				t.Fatalf("verify: %v", err)
			}
			input := routineMap(t, vector.DTO)
			delete(input, "content_hash")
			if string(routineCanonical(t, input)) != vector.CanonicalContent {
				t.Fatal("canonical bytes differ from independent Python fixture")
			}
			built, err := protocol.BuildRoutineVersion(routineJSON(t, input), crypto.JSONCanonicalizer{})
			if err != nil || !reflect.DeepEqual(built, verified) {
				t.Fatalf("build roundtrip: %v", err)
			}
			if _, err := protocol.BuildRoutineVersion(vector.DTO, crypto.JSONCanonicalizer{}); !errors.Is(err, protocol.ErrRoutineVersionInvalid) {
				t.Fatal("builder accepted existing hash", err)
			}
			if hashes[verified.ContentHash] {
				t.Fatal("different exact material shared a digest")
			}
			hashes[verified.ContentHash] = true
		})
	}
	for _, vector := range fixture.Invalid {
		t.Run(vector.Name, func(t *testing.T) {
			want := protocol.ErrRoutineVersionInvalid
			if vector.Error == "HASH" {
				want = protocol.ErrRoutineVersionHash
			}
			value, err := protocol.DecodeRoutineVersion(vector.DTO, crypto.JSONCanonicalizer{})
			if !errors.Is(err, want) || !reflect.DeepEqual(value, protocol.RoutineVersionDTO{}) {
				t.Fatalf("invalid result: %v", err)
			}
			if vector.Error == "INVALID" && vector.Name != "hash-uppercase" {
				input := routineMap(t, vector.DTO)
				delete(input, "content_hash")
				if _, err := protocol.BuildRoutineVersion(routineJSON(t, input), crypto.JSONCanonicalizer{}); !errors.Is(err, want) {
					t.Fatal("builder accepted invalid shape", err)
				}
			}
		})
	}
}

func TestRoutineVersionHashBindsEveryVariableField(t *testing.T) {
	raw := routineVectors(t).Valid[0].DTO
	day := func(v map[string]any) map[string]any { return v["days"].([]any)[0].(map[string]any) }
	exercise := func(v map[string]any) map[string]any { return day(v)["exercises"].([]any)[0].(map[string]any) }
	otherID := "10000000-0000-4000-8000-000000000999"
	mutations := map[string]func(map[string]any){
		"id":                    func(v map[string]any) { v["id"] = otherID },
		"routine_id":            func(v map[string]any) { v["routine_id"] = otherID },
		"version_number":        func(v map[string]any) { v["version_number"] = 8 },
		"title_snapshot":        func(v map[string]any) { v["title_snapshot"] = " " + v["title_snapshot"].(string) },
		"source_draft_revision": func(v map[string]any) { v["source_draft_revision"] = "9007199254740994" },
		"published_at":          func(v map[string]any) { v["published_at"] = "2026-10-19T12:34:56Z" },
		"days_order":            func(v map[string]any) { a := v["days"].([]any); a[0], a[1] = a[1], a[0] },
		"day_id":                func(v map[string]any) { day(v)["id"] = otherID },
		"day_label":             func(v map[string]any) { day(v)["label"] = "changed" },
		"day_position":          func(v map[string]any) { day(v)["position"] = 13 },
		"exercise_order":        func(v map[string]any) { a := day(v)["exercises"].([]any); a[0], a[1] = a[1], a[0] },
		"exercise_id":           func(v map[string]any) { exercise(v)["id"] = otherID },
		"exercise_ref_id":       func(v map[string]any) { exercise(v)["exercise_ref"].(map[string]any)["id"] = otherID },
		"exercise_ref_source":   func(v map[string]any) { exercise(v)["exercise_ref"].(map[string]any)["source"] = "personal" },
		"name_snapshot":         func(v map[string]any) { exercise(v)["name_snapshot"] = "changed" },
		"load_mode_snapshot":    func(v map[string]any) { exercise(v)["load_mode_snapshot"] = "none" },
		"exercise_position":     func(v map[string]any) { exercise(v)["position"] = 49 },
		"prescribed_sets":       func(v map[string]any) { exercise(v)["prescribed_sets"] = 100 },
		"prescribed_reps":       func(v map[string]any) { exercise(v)["prescribed_reps"] = "8–12 " },
		"rest_seconds":          func(v map[string]any) { exercise(v)["rest_seconds"] = 0 },
		"notes":                 func(v map[string]any) { exercise(v)["notes"] = "changed" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			value := routineMap(t, raw)
			mutate(value)
			if _, err := protocol.DecodeRoutineVersion(routineJSON(t, value), crypto.JSONCanonicalizer{}); !errors.Is(err, protocol.ErrRoutineVersionHash) {
				t.Fatal("field escaped digest", err)
			}
			delete(value, "content_hash")
			built, err := protocol.BuildRoutineVersion(routineJSON(t, value), crypto.JSONCanonicalizer{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = protocol.DecodeRoutineVersion(routineJSON(t, built), crypto.JSONCanonicalizer{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type brokenRoutineCanonicalizer struct{}

func (brokenRoutineCanonicalizer) Canonicalize([]byte) ([]byte, error) {
	return nil, errors.New("raw private diagnostic")
}
func TestRoutineVersionRejectsMalformedJSONAndCanonicalizerFailure(t *testing.T) {
	raw := routineVectors(t).Valid[0].DTO
	invalid := [][]byte{
		append(append([]byte{}, raw...), []byte(" {}")...), []byte("null"), []byte("[]"),
		[]byte(strings.Replace(string(raw), `"content_hash":`, `"content_hash":"`+strings.Repeat("a", 64)+`","content_hash":`, 1)),
		[]byte(strings.Replace(string(raw), "Fuerza", `\ud800`, 1)),
		[]byte(strings.Replace(string(raw), "Fuerza", string([]byte{0xff}), 1)),
	}
	for _, value := range invalid {
		if _, err := protocol.DecodeRoutineVersion(value, crypto.JSONCanonicalizer{}); !errors.Is(err, protocol.ErrRoutineVersionInvalid) {
			t.Fatal("malformed JSON accepted", err)
		}
	}
	for _, canonicalizer := range []protocol.Canonicalizer{nil, brokenRoutineCanonicalizer{}} {
		if _, err := protocol.DecodeRoutineVersion(raw, canonicalizer); err != protocol.ErrRoutineVersionInvalid {
			t.Fatal("canonicalizer failure escaped", err)
		}
	}
}

// Synthetic maximum-shape documents only; duplicate fixture IDs do not claim
// business-valid publications. The producer must validate those invariants.
func routineAtSize(t *testing.T, target int) map[string]any {
	t.Helper()
	value := routineMap(t, routineVectors(t).Valid[0].DTO)
	exercise := value["days"].([]any)[0].(map[string]any)["exercises"].([]any)[0].(map[string]any)
	exercise["notes"] = ""
	days := make([]any, 14)
	for i := range days {
		exercises := make([]any, 50)
		for j := range exercises {
			exercises[j] = routineMap(t, routineJSON(t, exercise))
		}
		days[i] = map[string]any{"id": value["id"], "label": "size", "position": i, "exercises": exercises}
	}
	value["days"] = days
	remaining := target - len(routineCanonical(t, value))
	if remaining < 0 {
		t.Fatal("target below scaffold")
	}
	for _, d := range days {
		for _, item := range d.(map[string]any)["exercises"].([]any) {
			size := min(remaining, 2000)
			item.(map[string]any)["notes"] = strings.Repeat("é", size/2) + strings.Repeat("x", size%2)
			remaining -= size
		}
	}
	if remaining != 0 || len(routineCanonical(t, value)) != target {
		t.Fatal("size fixture cannot reach exact bytes")
	}
	return value
}
func TestRoutineVersionFullDTOCanonicalByteLimit(t *testing.T) {
	for _, extra := range []int{0, 1} {
		value := routineAtSize(t, protocol.RoutineVersionMaxBytes+extra)
		delete(value, "content_hash")
		built, err := protocol.BuildRoutineVersion(routineJSON(t, value), crypto.JSONCanonicalizer{})
		if extra == 1 {
			if !errors.Is(err, protocol.ErrRoutineVersionTooLarge) {
				t.Fatal("full DTO limit missed hash overhead", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(routineCanonical(t, built)) != protocol.RoutineVersionMaxBytes {
			t.Fatal("wrong full DTO byte limit")
		}
		if _, err := protocol.DecodeRoutineVersion(routineJSON(t, built), crypto.JSONCanonicalizer{}); err != nil {
			t.Fatal(err)
		}
	}
}
