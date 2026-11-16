package workout

import (
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

func startFixture() (*routine.Version, StartCommand, time.Time) {
	now := time.Date(2026, 10, 19, 1, 2, 3, 456000000, time.UTC)
	v := &routine.Version{ID: uuid.New(), AccountID: uuid.New(), RoutineID: uuid.New(), ActorDeviceID: uuid.New(), VersionNumber: 1, TitleSnapshot: " Rutina e\u0301 <&> ", SourceDraftRevision: 2, ContentHash: [32]byte{1}, PublishedAt: now, Days: []routine.PublishedDay{}}
	for i := 0; i < 2; i++ {
		day := routine.PublishedDay{ID: uuid.New(), Label: " Día ", Position: i, Exercises: []routine.PublishedExercise{}}
		for j := 0; j < 2; j++ {
			rest := 0
			day.Exercises = append(day.Exercises, routine.PublishedExercise{ID: uuid.New(), Reference: routine.ExerciseRef{Source: "personal", ID: uuid.NewString()}, NameSnapshot: " Press ", LoadModeSnapshot: "external", Position: j, PrescribedSets: 3, PrescribedReps: " 8–12 ", RestSeconds: &rest, Notes: " A\n<&> "})
		}
		v.Days = append(v.Days, day)
	}
	zero := "0"
	c := StartCommand{OperationID: uuid.NewString(), OriginDeviceID: uuid.NewString(), EntityID: uuid.NewString(), Type: StartCommandType, Base: protocol.Base{Revision: &zero}, CapturedAt: "2099-01-01T01:02:03Z", Payload: StartPayload{RoutineVersionID: v.ID.String(), RoutineDayID: v.Days[0].ID.String(), VersionHash: hex.EncodeToString(v.ContentHash[:]), StartedAt: "2020-11-01T05:30:00Z", Timezone: "America/New_York", Exercises: []ExerciseMap{}}}
	for _, e := range v.Days[0].Exercises {
		c.Payload.Exercises = append(c.Payload.Exercises, ExerciseMap{SourceExerciseID: e.ID.String(), ID: uuid.NewString()})
	}
	return v, c, now
}
func TestWorkoutStartCopiesVersionAndPreservesIndependentTimes(t *testing.T) {
	v, c, now := startFixture()
	c.Payload.Exercises[0], c.Payload.Exercises[1] = c.Payload.Exercises[1], c.Payload.Exercises[0]
	original, _ := json.Marshal(c)
	w, err := NewStarted(uuid.MustParse(c.EntityID), v.AccountID, uuid.MustParse(c.OriginDeviceID), c.Payload, v, now)
	if err != nil {
		t.Fatal(err)
	}
	if !w.MatchesVersion(v) || w.State != "in_progress" || w.Revision != 1 || w.FinishedAt != nil || w.NeedsReview || w.FinalizedRevision != nil || !w.CreatedAt.Equal(now) || !w.StartedAt.Equal(time.Date(2020, 11, 1, 5, 30, 0, 0, time.UTC)) {
		t.Fatal("incorrect initial aggregate")
	}
	if w.Exercises[0].ID.String() != c.Payload.Exercises[1].ID || w.Exercises[0].SourceExerciseID != v.Days[0].Exercises[0].ID || w.Exercises[0].Notes != v.Days[0].Exercises[0].Notes {
		t.Fatal("snapshot/order changed")
	}
	dto := w.DTO()
	if dto.StartedAt != "2020-11-01T05:30:00.000Z" || dto.CreatedAt != "2026-10-19T01:02:03.456Z" || dto.Timezone != c.Payload.Timezone {
		t.Fatal("timestamps changed")
	}
	*w.Exercises[0].RestSeconds = 10
	if *v.Days[0].Exercises[0].RestSeconds != 0 {
		t.Fatal("source pointer aliased")
	}
	*dto.Exercises[0].RestSeconds = 20
	if *w.Exercises[0].RestSeconds != 10 {
		t.Fatal("DTO pointer aliased")
	}
	again, _ := json.Marshal(c)
	if string(original) != string(again) {
		t.Fatal("request changed")
	}
	// A valid, clearly incorrect device clock is still the supplied instant.
	c.Payload.StartedAt = "0001-01-01T00:00:00Z"
	if _, err = NewStarted(uuid.MustParse(c.EntityID), v.AccountID, uuid.MustParse(c.OriginDeviceID), c.Payload, v, now); err != nil {
		t.Fatal("invented clock range", err)
	}
}
func TestWorkoutStartRejectsNonBijectiveMapsAndForeignMaterial(t *testing.T) {
	for name, mutate := range map[string]func(*StartCommand){
		"missing": func(c *StartCommand) { c.Payload.Exercises = c.Payload.Exercises[:1] },
		"duplicate_source": func(c *StartCommand) {
			c.Payload.Exercises[1].SourceExerciseID = c.Payload.Exercises[0].SourceExerciseID
		},
		"duplicate_destination": func(c *StartCommand) { c.Payload.Exercises[1].ID = c.Payload.Exercises[0].ID },
		"existing_source":       func(c *StartCommand) { c.Payload.Exercises[0].ID = c.Payload.Exercises[1].SourceExerciseID },
		"unknown_source":        func(c *StartCommand) { c.Payload.Exercises[0].SourceExerciseID = uuid.NewString() },
		"wrong_day":             func(c *StartCommand) { c.Payload.RoutineDayID = uuid.NewString() },
		"wrong_version":         func(c *StartCommand) { c.Payload.RoutineVersionID = uuid.NewString() },
		"wrong_hash":            func(c *StartCommand) { c.Payload.VersionHash = strings.Repeat("0", 64) },
		"root_reuse":            func(c *StartCommand) { c.EntityID = c.Payload.RoutineVersionID },
	} {
		t.Run(name, func(t *testing.T) {
			v, c, now := startFixture()
			mutate(&c)
			if _, err := NewStarted(uuid.MustParse(c.EntityID), v.AccountID, uuid.MustParse(c.OriginDeviceID), c.Payload, v, now); err == nil {
				t.Fatal("invalid mapping accepted")
			}
		})
	}
	v, c, now := startFixture()
	if _, err := NewStarted(uuid.New(), uuid.New(), uuid.MustParse(c.OriginDeviceID), c.Payload, v, now); err == nil {
		t.Fatal("foreign version accepted")
	}
}
func TestWorkoutStartClosedCommandDoesNotNormalizeHashMaterial(t *testing.T) {
	_, c, _ := startFixture()
	raw, _ := json.Marshal(c)
	var decoded StartCommand
	if json.Unmarshal(raw, &decoded) != nil || !reflect.DeepEqual(c, decoded) {
		t.Fatal("roundtrip changed command")
	}
	for _, body := range []string{strings.Replace(string(raw), `"timezone":`, `"extra":true,"timezone":`, 1), strings.Replace(string(raw), `"timezone":`, `"timezone":"UTC","timezone":`, 1), strings.Replace(string(raw), `"timezone":"America/New_York"`, `"timezone":null`, 1), strings.Replace(string(raw), `"exercises":[`, `"exercises":[null,`, 1), strings.Replace(string(raw), c.Payload.StartedAt, "2020-02-30T05:30:00Z", 1), strings.Replace(string(raw), c.Payload.StartedAt, "2020-11-01T05:30:00.00Z", 1), strings.Replace(string(raw), c.Payload.VersionHash, strings.Repeat("A", 64), 1)} {
		if json.Unmarshal([]byte(body), &decoded) == nil {
			t.Fatal("invalid closed variant accepted")
		}
	}
	changed := c
	changed.CapturedAt = "2099-01-01T01:02:03.000Z"
	a, _ := json.Marshal(c)
	b, _ := json.Marshal(changed)
	if string(a) == string(b) {
		t.Fatal("capture text normalized")
	}
}
