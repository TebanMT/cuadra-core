package routine

import (
	"encoding/json"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/training/domain/commandjson"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

const PublishCommandType = "routine.publish"

type ExerciseIDMap struct {
	DraftExerciseID string `json:"draft_exercise_id"`
	ID              string `json:"id"`
}
type DayIDMap struct {
	DraftDayID string          `json:"draft_day_id"`
	ID         string          `json:"id"`
	Exercises  []ExerciseIDMap `json:"exercises"`
}
type PublishPayload struct {
	DraftRevision string     `json:"draft_revision"`
	VersionID     string     `json:"version_id"`
	Days          []DayIDMap `json:"days"`
}
type PublishCommand struct {
	OperationID    string         `json:"operation_id"`
	OriginDeviceID string         `json:"origin_device_id"`
	Type           string         `json:"type"`
	EntityID       string         `json:"entity_id"`
	Base           protocol.Base  `json:"base"`
	CapturedAt     string         `json:"captured_at"`
	Payload        PublishPayload `json:"payload"`
}

func (p PublishPayload) ValidateShape() error {
	if _, err := protocol.ParseRevision(p.DraftRevision); err != nil {
		return protocol.ErrCommandInvalid
	}
	if !validID(p.VersionID) || p.Days == nil || len(p.Days) < 1 || len(p.Days) > 14 {
		return protocol.ErrCommandInvalid
	}
	for _, day := range p.Days {
		if !validID(day.DraftDayID) || !validID(day.ID) || day.Exercises == nil || len(day.Exercises) < 1 || len(day.Exercises) > 50 {
			return protocol.ErrCommandInvalid
		}
		for _, e := range day.Exercises {
			if !validID(e.DraftExerciseID) || !validID(e.ID) {
				return protocol.ErrCommandInvalid
			}
		}
	}
	return nil
}
func (c PublishCommand) ValidateEnvelope() error {
	if !validID(c.OperationID) || !validID(c.OriginDeviceID) || !validID(c.EntityID) || c.Type != PublishCommandType || c.Base.Validate(uuid.MustParse(c.OperationID)) != nil || c.Payload.ValidateShape() != nil {
		return protocol.ErrCommandInvalid
	}
	format := "2006-01-02T15:04:05Z"
	if len(c.CapturedAt) == 24 {
		format = "2006-01-02T15:04:05.000Z"
	}
	at, err := time.Parse(format, c.CapturedAt)
	if err != nil || at.Year() < 1 || at.Format(format) != c.CapturedAt {
		return protocol.ErrCommandInvalid
	}
	return nil
}
func (c *PublishCommand) UnmarshalJSON(raw []byte) error {
	f, err := commandjson.Object(raw, "operation_id", "origin_device_id", "type", "entity_id", "base", "captured_at", "payload")
	if err != nil {
		return err
	}
	var next PublishCommand
	for key, target := range map[string]*string{"operation_id": &next.OperationID, "origin_device_id": &next.OriginDeviceID, "type": &next.Type, "entity_id": &next.EntityID, "captured_at": &next.CapturedAt} {
		if commandjson.Text(f[key], target) != nil {
			return protocol.ErrCommandInvalid
		}
	}
	if json.Unmarshal(f["base"], &next.Base) != nil {
		return protocol.ErrCommandInvalid
	}
	p, err := commandjson.Object(f["payload"], "draft_revision", "version_id", "days")
	if err != nil {
		return err
	}
	if commandjson.Text(p["draft_revision"], &next.Payload.DraftRevision) != nil || commandjson.Text(p["version_id"], &next.Payload.VersionID) != nil || array(p["days"], &next.Payload.Days) != nil || next.ValidateEnvelope() != nil {
		return protocol.ErrCommandInvalid
	}
	*c = next
	return nil
}
func (d *DayIDMap) UnmarshalJSON(raw []byte) error {
	f, err := commandjson.Object(raw, "draft_day_id", "id", "exercises")
	if err != nil {
		return err
	}
	var next DayIDMap
	if commandjson.Text(f["draft_day_id"], &next.DraftDayID) != nil || commandjson.Text(f["id"], &next.ID) != nil || array(f["exercises"], &next.Exercises) != nil {
		return protocol.ErrCommandInvalid
	}
	*d = next
	return nil
}
func (e *ExerciseIDMap) UnmarshalJSON(raw []byte) error {
	f, err := commandjson.Object(raw, "draft_exercise_id", "id")
	if err != nil {
		return err
	}
	var next ExerciseIDMap
	if commandjson.Text(f["draft_exercise_id"], &next.DraftExerciseID) != nil || commandjson.Text(f["id"], &next.ID) != nil {
		return protocol.ErrCommandInvalid
	}
	*e = next
	return nil
}
