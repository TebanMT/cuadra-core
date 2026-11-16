package account

import (
	"time"

	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
)

// ProfileCommand is the exact closed profile.update variant of protocol 1.
// CapturedAt remains its original string: retries do not normalize payloads.
type ProfileCommand struct {
	OperationID    string        `json:"operation_id"`
	OriginDeviceID string        `json:"origin_device_id"`
	Type           string        `json:"type"`
	EntityID       string        `json:"entity_id"`
	Base           protocol.Base `json:"base"`
	CapturedAt     string        `json:"captured_at"`
	Payload        struct {
		DisplayName string     `json:"display_name"`
		WeightUnit  WeightUnit `json:"weight_unit"`
	} `json:"payload"`
}

func (c ProfileCommand) ValidateEnvelope() error {
	for _, value := range []string{c.OperationID, c.OriginDeviceID, c.EntityID} {
		id, err := uuid.Parse(value)
		if err != nil || !protocol.ValidID(id) || id.String() != value {
			return protocol.ErrCommandInvalid
		}
	}
	if c.Type != "profile.update" {
		return protocol.ErrCommandInvalid
	}
	if err := c.Base.Validate(uuid.MustParse(c.OperationID)); err != nil {
		return err
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
