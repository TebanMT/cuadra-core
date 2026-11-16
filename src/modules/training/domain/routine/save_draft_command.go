package routine

import (
	"encoding/json"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/training/domain/commandjson"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

const SaveDraftCommandType = "routine.draft.save"

type SaveDraftPayload struct {
	Content       Draft   `json:"content"`
	BaseVersionID *string `json:"base_version_id"`
}
type SaveDraftCommand struct {
	OperationID    string           `json:"operation_id"`
	OriginDeviceID string           `json:"origin_device_id"`
	Type           string           `json:"type"`
	EntityID       string           `json:"entity_id"`
	Base           protocol.Base    `json:"base"`
	CapturedAt     string           `json:"captured_at"`
	Payload        SaveDraftPayload `json:"payload"`
}

func (c SaveDraftCommand) ValidateEnvelope() error {
	if !validID(c.OperationID) || !validID(c.OriginDeviceID) || !validID(c.EntityID) || c.Type != SaveDraftCommandType || c.Base.Validate(uuid.MustParse(c.OperationID)) != nil {
		return protocol.ErrCommandInvalid
	}
	format := "2006-01-02T15:04:05Z"
	if len(c.CapturedAt) == 24 {
		format = "2006-01-02T15:04:05.000Z"
	}
	at, err := time.Parse(format, c.CapturedAt)
	if err != nil || at.Year() < 1 || at.Format(format) != c.CapturedAt || c.Payload.Content.Validate() != nil || (c.Payload.BaseVersionID != nil && !validID(*c.Payload.BaseVersionID)) {
		return protocol.ErrCommandInvalid
	}
	return nil
}
func (c *SaveDraftCommand) UnmarshalJSON(raw []byte) error {
	fields, err := commandjson.Object(raw, "operation_id", "origin_device_id", "type", "entity_id", "base", "captured_at", "payload")
	if err != nil {
		return err
	}
	var next SaveDraftCommand
	for key, target := range map[string]*string{"operation_id": &next.OperationID, "origin_device_id": &next.OriginDeviceID, "type": &next.Type, "entity_id": &next.EntityID, "captured_at": &next.CapturedAt} {
		if commandjson.Text(fields[key], target) != nil {
			return protocol.ErrCommandInvalid
		}
	}
	if json.Unmarshal(fields["base"], &next.Base) != nil {
		return protocol.ErrCommandInvalid
	}
	payload, err := commandjson.Object(fields["payload"], "content", "base_version_id")
	if err != nil {
		return err
	}
	if json.Unmarshal(payload["content"], &next.Payload.Content) != nil || json.Unmarshal(payload["base_version_id"], &next.Payload.BaseVersionID) != nil || next.ValidateEnvelope() != nil {
		return protocol.ErrCommandInvalid
	}
	*c = next
	return nil
}
