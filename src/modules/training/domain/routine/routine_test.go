package routine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

func incompleteDraft() Draft {
	return Draft{FormatVersion: 1, Title: "", Days: []DraftDay{{ID: uuid.NewString(), Label: "", Exercises: []DraftExercise{{ID: uuid.NewString(), PrescribedReps: "", Notes: ""}}}}}
}
func TestRoutineCreationPreservesIncompleteDraftWithoutPublication(t *testing.T) {
	content := incompleteDraft()
	content.Title = "  Rutina Élite 漢  "
	sets, rest := 3, 0
	content.Days[0].Exercises[0].PrescribedSets = &sets
	content.Days[0].Exercises[0].RestSeconds = &rest
	id, owner := uuid.New(), uuid.New()
	r, d, err := NewDraftRoutine(id, owner, content, time.Date(2026, 10, 19, 1, 0, 0, 123456789, time.FixedZone("synthetic", 3600)))
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id || d.RoutineID != id || r.AccountID != owner || d.AccountID != owner || r.Title != content.Title || r.Revision != 1 || d.Revision != 1 || r.Status != "draft" || r.CurrentVersionID != nil || r.DuplicatedFromVersionID != nil || d.BaseVersionID != nil || d.Content.Days[0].Exercises[0].ExerciseRef != nil || r.CreatedAt.Location() != time.UTC || r.CreatedAt.Nanosecond() != 123000000 {
		t.Fatal("creation published or completed draft")
	}
	content.Days[0].Label = "Changed"
	sets = 99
	if d.Content.Days[0].Label != "" || *d.Content.Days[0].Exercises[0].PrescribedSets != 3 {
		t.Fatal("aggregate aliased input")
	}
	if ValidateCreation(uuid.New(), r, d) == nil {
		t.Fatal("foreign owner accepted")
	}
	empty := Draft{FormatVersion: 1, Days: []DraftDay{}}
	if _, _, err = NewDraftRoutine(uuid.New(), owner, empty, time.Now()); err != nil {
		t.Fatal("empty draft rejected", err)
	}
}
func TestRoutineDraftBoundsAndUniqueEditingIDs(t *testing.T) {
	valid := incompleteDraft()
	for _, change := range []func(*Draft){
		func(d *Draft) { d.Days = nil },
		func(d *Draft) { d.Days[0].Exercises = nil },
		func(d *Draft) { d.Days[0].Exercises[0].ID = d.Days[0].ID },
		func(d *Draft) { d.Days = append(d.Days, d.Days[0]) },
		func(d *Draft) { d.Days[0].Exercises = append(d.Days[0].Exercises, d.Days[0].Exercises[0]) },
		func(d *Draft) { n := 0; d.Days[0].Exercises[0].PrescribedSets = &n },
		func(d *Draft) { n := 3601; d.Days[0].Exercises[0].RestSeconds = &n },
		func(d *Draft) { d.Title = strings.Repeat("漢", 121) },
		func(d *Draft) { d.Days[0].Exercises[0].Notes = "a\x00b" },
		func(d *Draft) {
			d.Days[0].Exercises[0].ExerciseRef = &ExerciseRef{Source: "other", ID: uuid.NewString()}
		},
	} {
		next := valid.Clone()
		change(&next)
		if next.Validate() == nil {
			t.Fatal("invalid editable content accepted")
		}
	}
	valid.Title = strings.Repeat("漢", 120)
	valid.Days[0].Label = strings.Repeat("😀", 80)
	valid.Days[0].Exercises[0].Notes = strings.Repeat("😀", 1000)
	if err := valid.Validate(); err != nil {
		t.Fatal("Unicode bounds counted bytes", err)
	}
}
func TestRoutineCreateCommandClosedNestedContract(t *testing.T) {
	zero := "0"
	c := CreateCommand{OperationID: uuid.NewString(), OriginDeviceID: uuid.NewString(), Type: CreateCommandType, EntityID: uuid.NewString(), Base: protocol.Base{Revision: &zero}, CapturedAt: "2026-10-19T00:00:00Z", Payload: CreatePayload{Draft: incompleteDraft()}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CreateCommand
	if json.Unmarshal(raw, &decoded) != nil || decoded.CapturedAt != c.CapturedAt || decoded.Payload.Draft.Days[0].Exercises[0].PrescribedSets != nil {
		t.Fatal("valid incomplete command changed")
	}
	for _, edit := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"notes":""`, `"notes":null`, 1) },
		func(s string) string { return strings.Replace(s, `,"notes":""`, "", 1) },
		func(s string) string { return strings.Replace(s, `"notes":""`, `"notes":"","notes":""`, 1) },
		func(s string) string { return strings.Replace(s, `"notes":""`, `"notes":"","unknown":0`, 1) },
		func(s string) string { return strings.Replace(s, `"format_version":1`, `"format_version":null`, 1) },
		func(s string) string { return strings.Replace(s, `"prescribed_sets":null`, `"prescribed_sets":1.0`, 1) },
		func(s string) string { return strings.Replace(s, `"notes":""`, `"notes":"\ud800"`, 1) },
		func(s string) string {
			return strings.Replace(s, `"exercise_ref":null`, `"exercise_ref":{"id":"`+uuid.NewString()+`","source":"personal","extra":1}`, 1)
		},
	} {
		if json.Unmarshal([]byte(edit(string(raw))), &decoded) == nil {
			t.Fatal("open/invalid nested JSON accepted")
		}
	}
}
