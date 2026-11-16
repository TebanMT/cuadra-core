package routine

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/training/domain/commandjson"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

const CreateCommandType = "routine.create"

type CreatePayload struct {
	Draft Draft `json:"draft"`
}
type CreateCommand struct {
	OperationID    string        `json:"operation_id"`
	OriginDeviceID string        `json:"origin_device_id"`
	Type           string        `json:"type"`
	EntityID       string        `json:"entity_id"`
	Base           protocol.Base `json:"base"`
	CapturedAt     string        `json:"captured_at"`
	Payload        CreatePayload `json:"payload"`
}

func (c CreateCommand) ValidateEnvelope() error {
	if !validID(c.OperationID) || !validID(c.OriginDeviceID) || !validID(c.EntityID) || c.Type != CreateCommandType || c.Base.Validate(uuid.MustParse(c.OperationID)) != nil {
		return protocol.ErrCommandInvalid
	}
	format := "2006-01-02T15:04:05Z"
	if len(c.CapturedAt) == 24 {
		format = "2006-01-02T15:04:05.000Z"
	}
	at, err := time.Parse(format, c.CapturedAt)
	if err != nil || at.Year() < 1 || at.Format(format) != c.CapturedAt || c.Payload.Draft.Validate() != nil {
		return protocol.ErrCommandInvalid
	}
	return nil
}
func (c *CreateCommand) UnmarshalJSON(raw []byte) error {
	fields, err := commandjson.Object(raw, "operation_id", "origin_device_id", "type", "entity_id", "base", "captured_at", "payload")
	if err != nil {
		return err
	}
	var next CreateCommand
	for key, target := range map[string]*string{"operation_id": &next.OperationID, "origin_device_id": &next.OriginDeviceID, "type": &next.Type, "entity_id": &next.EntityID, "captured_at": &next.CapturedAt} {
		if commandjson.Text(fields[key], target) != nil {
			return protocol.ErrCommandInvalid
		}
	}
	if json.Unmarshal(fields["base"], &next.Base) != nil {
		return protocol.ErrCommandInvalid
	}
	payload, err := commandjson.Object(fields["payload"], "draft")
	if err != nil {
		return err
	}
	if json.Unmarshal(payload["draft"], &next.Payload.Draft) != nil || next.ValidateEnvelope() != nil {
		return protocol.ErrCommandInvalid
	}
	*c = next
	return nil
}

func (d *Draft) UnmarshalJSON(raw []byte) error {
	fields, err := commandjson.Object(raw, "format_version", "title", "days")
	if err != nil {
		return err
	}
	var next Draft
	if json.Unmarshal(fields["format_version"], &next.FormatVersion) != nil || commandjson.Text(fields["title"], &next.Title) != nil || array(fields["days"], &next.Days) != nil || next.Validate() != nil {
		return protocol.ErrCommandInvalid
	}
	*d = next
	return nil
}
func (d *DraftDay) UnmarshalJSON(raw []byte) error {
	fields, err := commandjson.Object(raw, "id", "label", "exercises")
	if err != nil {
		return err
	}
	var next DraftDay
	if commandjson.Text(fields["id"], &next.ID) != nil || commandjson.Text(fields["label"], &next.Label) != nil || array(fields["exercises"], &next.Exercises) != nil {
		return protocol.ErrCommandInvalid
	}
	*d = next
	return nil
}
func (e *DraftExercise) UnmarshalJSON(raw []byte) error {
	fields, err := commandjson.Object(raw, "id", "exercise_ref", "prescribed_sets", "prescribed_reps", "rest_seconds", "notes")
	if err != nil {
		return err
	}
	var next DraftExercise
	for key, target := range map[string]*string{"id": &next.ID, "prescribed_reps": &next.PrescribedReps, "notes": &next.Notes} {
		if commandjson.Text(fields[key], target) != nil {
			return protocol.ErrCommandInvalid
		}
	}
	if json.Unmarshal(fields["exercise_ref"], &next.ExerciseRef) != nil || json.Unmarshal(fields["prescribed_sets"], &next.PrescribedSets) != nil || json.Unmarshal(fields["rest_seconds"], &next.RestSeconds) != nil {
		return protocol.ErrCommandInvalid
	}
	*e = next
	return nil
}
func (r *ExerciseRef) UnmarshalJSON(raw []byte) error {
	fields, err := commandjson.Object(raw, "source", "id")
	if err != nil {
		return err
	}
	var next ExerciseRef
	if commandjson.Text(fields["source"], &next.Source) != nil || commandjson.Text(fields["id"], &next.ID) != nil {
		return protocol.ErrCommandInvalid
	}
	*r = next
	return nil
}
func array(raw []byte, target any) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '[' {
		return protocol.ErrCommandInvalid
	}
	if json.Unmarshal(raw, target) != nil {
		return protocol.ErrCommandInvalid
	}
	return nil
}
