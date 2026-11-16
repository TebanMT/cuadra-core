package workout

import (
	"encoding/json"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func setDomainFixture(t *testing.T) (*Workout, CreateSetCommand, time.Time) {
	t.Helper()
	v, c, now := startFixture()
	w, err := NewStarted(uuid.MustParse(c.EntityID), v.AccountID, uuid.MustParse(c.OriginDeviceID), c.Payload, v, now)
	if err != nil {
		t.Fatal(err)
	}
	zero, one, value, unit := "0", "1", "30.0", "kg"
	s := CreateSetCommand{OperationID: uuid.NewString(), OriginDeviceID: w.OriginDeviceID.String(), Type: CreateSetCommandType, EntityID: uuid.NewString(), Base: protocol.Base{Revision: &zero}, CapturedAt: "2099-10-19T10:00:00Z", Payload: CreateSetPayload{WorkoutID: w.ID.String(), WorkoutExerciseID: w.Exercises[0].ID.String(), ObservedWorkout: protocol.Base{Revision: &one}, PositionHint: 0, Load: LoadInput{LoadMode: "external", WeightValue: &value, WeightUnit: &unit}, Repetitions: 0}}
	return w, s, now.Add(time.Second)
}
func TestWorkoutSetKeepsExactDecimalAndIndependentCapture(t *testing.T) {
	w, c, now := setDomainFixture(t)
	raw, _ := json.Marshal(c)
	captured, _ := parseTimestamp(c.CapturedAt)
	s, err := NewSet(w, uuid.MustParse(c.EntityID), w.OriginDeviceID, w.Exercises[0].ID, 0, 0, Load{LoadMode: c.Payload.Load.LoadMode, WeightValue: c.Payload.Load.WeightValue, WeightUnit: c.Payload.Load.WeightUnit}, captured, now)
	if err != nil {
		t.Fatal(err)
	}
	if *s.Load.WeightValue != "30.000" || *c.Payload.Load.WeightValue != "30.0" || !s.CapturedAt.Equal(captured) || !s.CreatedAt.Equal(now) || s.Repetitions != 0 {
		t.Fatal("decimal or independent capture changed")
	}
	next, err := w.WithNewSet(s, now)
	if err != nil || next.Revision != 2 || w.Revision != 1 || next.NeedsReview {
		t.Fatal("incorrect active workout effect", err)
	}
	dto := s.DTO()
	*dto.Load.WeightValue = "99.000"
	if *s.Load.WeightValue != "30.000" {
		t.Fatal("DTO aliases entity")
	}
	again, _ := json.Marshal(c)
	if string(raw) != string(again) {
		t.Fatal("request normalized")
	}
	*next.Exercises[0].RestSeconds = 77
	if *w.Exercises[0].RestSeconds == 77 {
		t.Fatal("snapshot aliased")
	}
	if _, err = NewSet(w, uuid.New(), w.OriginDeviceID, w.Exercises[0].ID, 0, 1, Load{LoadMode: "none"}, captured, now); err == nil {
		t.Fatal("implicit load mode override")
	}
}
func TestWorkoutSetRetainsFinalizationAndRejectsDeletedOrOverflow(t *testing.T) {
	w, c, now := setDomainFixture(t)
	load, _ := CanonicalLoad(c.Payload.Load.LoadMode, c.Payload.Load.WeightValue, c.Payload.Load.WeightUnit)
	captured, _ := parseTimestamp(c.CapturedAt)
	s, err := NewSet(w, uuid.New(), w.OriginDeviceID, w.Exercises[0].ID, 0, 1, load, captured, now)
	if err != nil {
		t.Fatal(err)
	}
	finish := w.StartedAt.Add(time.Hour)
	finalized := int64(2)
	w.State = "completed"
	w.Revision = 2
	w.FinishedAt = &finish
	w.FinalizedRevision = &finalized
	next, err := w.WithNewSet(s, now)
	if err != nil || next.Revision != 3 || !next.NeedsReview || next.State != "completed" || !next.FinishedAt.Equal(finish) || *next.FinalizedRevision != 2 {
		t.Fatal("finalization overwritten", err)
	}
	*next.FinalizedRevision = 1
	if *w.FinalizedRevision != 2 {
		t.Fatal("finalized pointer aliased")
	}
	w.DeletedAt = &now
	if _, err = w.WithNewSet(s, now); err == nil {
		t.Fatal("deleted aggregate resurrected")
	}
	w.DeletedAt = nil
	w.Revision = math.MaxInt64
	if _, err = w.WithNewSet(s, now); err != trainingErrors.ErrTrainingLimit {
		t.Fatal("revision overflow accepted", err)
	}
}
func TestWorkoutSetClosedLoadAndObservedDependency(t *testing.T) {
	_, c, _ := setDomainFixture(t)
	raw, _ := json.Marshal(c)
	var decoded CreateSetCommand
	if json.Unmarshal(raw, &decoded) != nil || !reflect.DeepEqual(c, decoded) {
		t.Fatal("closed roundtrip changed command")
	}
	for _, body := range []string{strings.Replace(string(raw), `"weight_value":"30.0"`, `"weight_value":30.0`, 1), strings.Replace(string(raw), `"weight_value":"30.0"`, `"weight_value":null`, 1), strings.Replace(string(raw), `"load_mode":"external"`, `"load_mode":"none"`, 1), strings.Replace(string(raw), `"repetitions":0`, `"repetitions":null`, 1), strings.Replace(string(raw), `"repetitions":0`, `"repetitions":1.5`, 1), strings.Replace(string(raw), `"position_hint":0`, `"position_hint":10000`, 1), strings.Replace(string(raw), `"load":{`, `"load":{"extra":true,`, 1), strings.Replace(string(raw), `"load":{`, `"load":{"weight_unit":"kg",`, 1)} {
		if json.Unmarshal([]byte(body), &decoded) == nil {
			t.Fatal("malformed set accepted")
		}
	}
	c.Payload.ObservedWorkout = protocol.Base{AfterOperationID: &c.OperationID}
	if c.ValidateEnvelope() == nil {
		t.Fatal("observed self-cycle accepted")
	}
	c.Payload.ObservedWorkout = protocol.Base{Revision: func() *string { s := "1"; return &s }()}
	c.Payload.Load = LoadInput{LoadMode: "none"}
	if c.ValidateEnvelope() != nil {
		t.Fatal("explicit none rejected by grammar")
	}
}
