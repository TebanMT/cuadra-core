package workout

import (
	"encoding/hex"
	"encoding/json"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/commandjson"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"time"
)

const StartCommandType = "workout.start"

type ExerciseMap struct {
	SourceExerciseID string `json:"source_exercise_id"`
	ID               string `json:"id"`
}
type StartPayload struct {
	RoutineVersionID string        `json:"routine_version_id"`
	RoutineDayID     string        `json:"routine_day_id"`
	VersionHash      string        `json:"version_hash"`
	StartedAt        string        `json:"started_at"`
	Timezone         string        `json:"timezone"`
	Exercises        []ExerciseMap `json:"exercises"`
}
type StartCommand struct {
	OperationID    string        `json:"operation_id"`
	OriginDeviceID string        `json:"origin_device_id"`
	Type           string        `json:"type"`
	EntityID       string        `json:"entity_id"`
	Base           protocol.Base `json:"base"`
	CapturedAt     string        `json:"captured_at"`
	Payload        StartPayload  `json:"payload"`
}

func canonicalID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && protocol.ValidID(id) && id.String() == s
}
func parseTimestamp(s string) (time.Time, error) {
	layout := "2006-01-02T15:04:05Z"
	if len(s) == 24 {
		layout = "2006-01-02T15:04:05.000Z"
	}
	at, err := time.Parse(layout, s)
	if err != nil || at.Year() < 1 || at.Format(layout) != s {
		return time.Time{}, protocol.ErrCommandInvalid
	}
	return at, nil
}
func (p StartPayload) validate() error {
	if !canonicalID(p.RoutineVersionID) || !canonicalID(p.RoutineDayID) || !validText(p.Timezone, 1, 80) || len(p.VersionHash) != 64 || len(p.Exercises) < 1 || len(p.Exercises) > 50 {
		return protocol.ErrCommandInvalid
	}
	hash, err := hex.DecodeString(p.VersionHash)
	if err != nil || hex.EncodeToString(hash) != p.VersionHash {
		return protocol.ErrCommandInvalid
	}
	if _, err = parseTimestamp(p.StartedAt); err != nil {
		return err
	}
	for _, m := range p.Exercises {
		if !canonicalID(m.ID) || !canonicalID(m.SourceExerciseID) {
			return protocol.ErrCommandInvalid
		}
	}
	return nil
}
func (c StartCommand) ValidateEnvelope() error {
	if !canonicalID(c.OperationID) || !canonicalID(c.OriginDeviceID) || !canonicalID(c.EntityID) || c.Type != StartCommandType {
		return protocol.ErrCommandInvalid
	}
	if c.Base.Validate(uuid.MustParse(c.OperationID)) != nil {
		return protocol.ErrCommandInvalid
	}
	if _, err := parseTimestamp(c.CapturedAt); err != nil {
		return err
	}
	return c.Payload.validate()
}
func (c *StartCommand) UnmarshalJSON(raw []byte) error {
	fields, err := commandjson.Object(raw, "operation_id", "origin_device_id", "type", "entity_id", "base", "captured_at", "payload")
	if err != nil {
		return err
	}
	var next StartCommand
	for k, p := range map[string]*string{"operation_id": &next.OperationID, "origin_device_id": &next.OriginDeviceID, "type": &next.Type, "entity_id": &next.EntityID, "captured_at": &next.CapturedAt} {
		if commandjson.Text(fields[k], p) != nil {
			return protocol.ErrCommandInvalid
		}
	}
	if json.Unmarshal(fields["base"], &next.Base) != nil {
		return protocol.ErrCommandInvalid
	}
	p, err := commandjson.Object(fields["payload"], "routine_version_id", "routine_day_id", "version_hash", "started_at", "timezone", "exercises")
	if err != nil {
		return err
	}
	for k, v := range map[string]*string{"routine_version_id": &next.Payload.RoutineVersionID, "routine_day_id": &next.Payload.RoutineDayID, "version_hash": &next.Payload.VersionHash, "started_at": &next.Payload.StartedAt, "timezone": &next.Payload.Timezone} {
		if commandjson.Text(p[k], v) != nil {
			return protocol.ErrCommandInvalid
		}
	}
	var mappings []json.RawMessage
	if json.Unmarshal(p["exercises"], &mappings) != nil || len(mappings) < 1 || len(mappings) > 50 {
		return protocol.ErrCommandInvalid
	}
	next.Payload.Exercises = make([]ExerciseMap, len(mappings))
	for i, raw := range mappings {
		m, e := commandjson.Object(raw, "source_exercise_id", "id")
		if e != nil {
			return e
		}
		if commandjson.Text(m["source_exercise_id"], &next.Payload.Exercises[i].SourceExerciseID) != nil || commandjson.Text(m["id"], &next.Payload.Exercises[i].ID) != nil {
			return protocol.ErrCommandInvalid
		}
	}
	if err = next.ValidateEnvelope(); err != nil {
		return err
	}
	*c = next
	return nil
}
