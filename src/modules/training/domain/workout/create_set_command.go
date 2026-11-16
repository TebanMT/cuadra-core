package workout

import (
	"encoding/json"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/commandjson"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

const CreateSetCommandType = "workout.set.create"

type LoadInput struct {
	LoadMode    string  `json:"load_mode"`
	WeightValue *string `json:"weight_value"`
	WeightUnit  *string `json:"weight_unit"`
}
type CreateSetPayload struct {
	WorkoutID         string        `json:"workout_id"`
	WorkoutExerciseID string        `json:"workout_exercise_id"`
	ObservedWorkout   protocol.Base `json:"observed_workout"`
	PositionHint      int           `json:"position_hint"`
	Load              LoadInput     `json:"load"`
	Repetitions       int           `json:"repetitions"`
}
type CreateSetCommand struct {
	OperationID    string           `json:"operation_id"`
	OriginDeviceID string           `json:"origin_device_id"`
	Type           string           `json:"type"`
	EntityID       string           `json:"entity_id"`
	Base           protocol.Base    `json:"base"`
	CapturedAt     string           `json:"captured_at"`
	Payload        CreateSetPayload `json:"payload"`
}

func (c CreateSetCommand) ValidateEnvelope() error {
	for _, s := range []string{c.OperationID, c.OriginDeviceID, c.EntityID, c.Payload.WorkoutID, c.Payload.WorkoutExerciseID} {
		if !canonicalID(s) {
			return protocol.ErrCommandInvalid
		}
	}
	op := uuid.MustParse(c.OperationID)
	if c.Type != CreateSetCommandType || c.Base.Validate(op) != nil || c.Payload.ObservedWorkout.Validate(op) != nil || c.Payload.PositionHint < 0 || c.Payload.PositionHint > 9999 || c.Payload.Repetitions < 0 || c.Payload.Repetitions > 10000 {
		return protocol.ErrCommandInvalid
	}
	if _, err := parseTimestamp(c.CapturedAt); err != nil {
		return err
	}
	if _, err := CanonicalLoad(c.Payload.Load.LoadMode, c.Payload.Load.WeightValue, c.Payload.Load.WeightUnit); err != nil {
		return protocol.ErrCommandInvalid
	}
	return nil
}
func nullableText(raw json.RawMessage) (*string, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var s string
	if commandjson.Text(raw, &s) != nil {
		return nil, protocol.ErrCommandInvalid
	}
	return &s, nil
}
func closedInt(raw json.RawMessage) (int, error) {
	var n *int
	if json.Unmarshal(raw, &n) != nil || n == nil {
		return 0, protocol.ErrCommandInvalid
	}
	return *n, nil
}
func (c *CreateSetCommand) UnmarshalJSON(raw []byte) error {
	f, err := commandjson.Object(raw, "operation_id", "origin_device_id", "type", "entity_id", "base", "captured_at", "payload")
	if err != nil {
		return err
	}
	var next CreateSetCommand
	for k, p := range map[string]*string{"operation_id": &next.OperationID, "origin_device_id": &next.OriginDeviceID, "type": &next.Type, "entity_id": &next.EntityID, "captured_at": &next.CapturedAt} {
		if commandjson.Text(f[k], p) != nil {
			return protocol.ErrCommandInvalid
		}
	}
	if json.Unmarshal(f["base"], &next.Base) != nil {
		return protocol.ErrCommandInvalid
	}
	p, err := commandjson.Object(f["payload"], "workout_id", "workout_exercise_id", "observed_workout", "position_hint", "load", "repetitions")
	if err != nil {
		return err
	}
	if commandjson.Text(p["workout_id"], &next.Payload.WorkoutID) != nil || commandjson.Text(p["workout_exercise_id"], &next.Payload.WorkoutExerciseID) != nil || json.Unmarshal(p["observed_workout"], &next.Payload.ObservedWorkout) != nil {
		return protocol.ErrCommandInvalid
	}
	if next.Payload.PositionHint, err = closedInt(p["position_hint"]); err != nil {
		return err
	}
	if next.Payload.Repetitions, err = closedInt(p["repetitions"]); err != nil {
		return err
	}
	load, err := commandjson.Object(p["load"], "load_mode", "weight_value", "weight_unit")
	if err != nil {
		return err
	}
	if commandjson.Text(load["load_mode"], &next.Payload.Load.LoadMode) != nil {
		return protocol.ErrCommandInvalid
	}
	if next.Payload.Load.WeightValue, err = nullableText(load["weight_value"]); err != nil {
		return err
	}
	if next.Payload.Load.WeightUnit, err = nullableText(load["weight_unit"]); err != nil {
		return err
	}
	if err = next.ValidateEnvelope(); err != nil {
		return err
	}
	*c = next
	return nil
}
