package workout

import (
	"encoding/json"

	"github.com/cuadra/cuadra-core/src/modules/training/domain/commandjson"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
)

// OfflineCommand is the closed union of mounted offline producers. It is not
// the wider contractual union: a producer needs an implementation before use.
type OfflineCommand struct {
	Start     *StartCommand
	CreateSet *CreateSetCommand
}

func (c OfflineCommand) ValidateEnvelope() error {
	if c.Start != nil && c.CreateSet == nil {
		return c.Start.ValidateEnvelope()
	}
	if c.CreateSet != nil && c.Start == nil {
		return c.CreateSet.ValidateEnvelope()
	}
	return protocol.ErrCommandInvalid
}

// Callers validate the union before reading identity or dispatching it.
func (c OfflineCommand) OperationID() string {
	if c.Start != nil {
		return c.Start.OperationID
	}
	if c.CreateSet != nil {
		return c.CreateSet.OperationID
	}
	return ""
}
func (c OfflineCommand) OriginDeviceID() string {
	if c.Start != nil {
		return c.Start.OriginDeviceID
	}
	if c.CreateSet != nil {
		return c.CreateSet.OriginDeviceID
	}
	return ""
}
func (c OfflineCommand) MarshalJSON() ([]byte, error) {
	if err := c.ValidateEnvelope(); err != nil {
		return nil, err
	}
	if c.Start != nil {
		return json.Marshal(c.Start)
	}
	return json.Marshal(c.CreateSet)
}
func (c *OfflineCommand) UnmarshalJSON(raw []byte) error {
	f, err := commandjson.Object(raw, "operation_id", "origin_device_id", "type", "entity_id", "base", "captured_at", "payload")
	if err != nil {
		return err
	}
	var kind string
	if commandjson.Text(f["type"], &kind) != nil {
		return protocol.ErrCommandInvalid
	}
	var next OfflineCommand
	switch kind {
	case StartCommandType:
		next.Start = new(StartCommand)
		err = json.Unmarshal(raw, next.Start)
	case CreateSetCommandType:
		next.CreateSet = new(CreateSetCommand)
		err = json.Unmarshal(raw, next.CreateSet)
	default:
		return protocol.ErrCommandInvalid
	}
	if err != nil {
		return err
	}
	if err = next.ValidateEnvelope(); err != nil {
		return err
	}
	*c = next
	return nil
}
