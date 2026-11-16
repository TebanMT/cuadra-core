package exercise

import (
	"encoding/json"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/training/domain/commandjson"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

const CreateCommandType = "personal_exercise.create"

type CreatePayload struct {
	Name     string   `json:"name"`
	LoadMode LoadMode `json:"load_mode"`
	Notes    string   `json:"notes"`
}

// Original strings are preserved for hashing. No trimming, timestamp rewriting
// or Unicode normalization may change a pending command's identity.
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
	for _, value := range []string{c.OperationID, c.OriginDeviceID, c.EntityID} {
		id, err := uuid.Parse(value)
		if err != nil || !validID(id) || id.String() != value {
			return protocol.ErrCommandInvalid
		}
	}
	if c.Type != CreateCommandType || c.Base.Validate(uuid.MustParse(c.OperationID)) != nil {
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
	if !validText(c.Payload.Name, 1, 120) || !validText(c.Payload.Notes, 0, 1000) || (c.Payload.LoadMode != External && c.Payload.LoadMode != None) {
		return protocol.ErrCommandInvalid
	}
	return nil
}

// Decode the exact closed variant, including required empty notes. Standard
// encoding/json alone accepts duplicate keys, null strings and absent fields.
func (c *CreateCommand) UnmarshalJSON(raw []byte) error {
	fields, err := commandjson.Object(raw, "operation_id", "origin_device_id", "type", "entity_id", "base", "captured_at", "payload")
	if err != nil {
		return err
	}
	var next CreateCommand
	for key, target := range map[string]*string{"operation_id": &next.OperationID, "origin_device_id": &next.OriginDeviceID, "type": &next.Type, "entity_id": &next.EntityID, "captured_at": &next.CapturedAt} {
		if err = commandjson.Text(fields[key], target); err != nil {
			return err
		}
	}
	if json.Unmarshal(fields["base"], &next.Base) != nil {
		return protocol.ErrCommandInvalid
	}
	payload, err := commandjson.Object(fields["payload"], "name", "load_mode", "notes")
	if err != nil {
		return err
	}
	var mode string
	if commandjson.Text(payload["name"], &next.Payload.Name) != nil || commandjson.Text(payload["load_mode"], &mode) != nil || commandjson.Text(payload["notes"], &next.Payload.Notes) != nil {
		return protocol.ErrCommandInvalid
	}
	next.Payload.LoadMode = LoadMode(mode)
	if err = next.ValidateEnvelope(); err != nil {
		return err
	}
	*c = next
	return nil
}
