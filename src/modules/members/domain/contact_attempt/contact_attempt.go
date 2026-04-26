// Package contact_attempt holds the ContactAttempt entity (UC-035). One row
// is appended every time the operator marks "Le hablé" in the persecución
// list. The row is append-only — there is no Update path. The aggregate's
// Member.LastContactAttemptAt cache is bumped by the same use case.
package contact_attempt

import (
	"strings"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
)

const (
	ChannelWhatsApp = "whatsapp"
	ChannelPhone    = "phone"
	ChannelInPerson = "in_person"
	ChannelOther    = "other"
)

// ContactAttempt mirrors the contact_attempts table.
type ContactAttempt struct {
	ID          uuid.UUID
	GymID       uuid.UUID
	Version     int
	MemberID    uuid.UUID
	AttemptAt   time.Time
	Channel     *string
	Note        *string
	ContactedBy uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// New constructs a ContactAttempt with full validation. Channel is optional
// (nil → operator did not specify); when present it must be one of the four
// canonical values. Note is also optional and capped at 1000 chars.
func New(id, gymID, memberID, contactedBy uuid.UUID, attemptAt time.Time,
	channel, note *string, now time.Time) (*ContactAttempt, error) {
	a := &ContactAttempt{
		ID:          id,
		GymID:       gymID,
		Version:     1,
		MemberID:    memberID,
		AttemptAt:   attemptAt,
		ContactedBy: contactedBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if channel != nil {
		v := strings.TrimSpace(*channel)
		if v == "" {
			a.Channel = nil
		} else {
			switch v {
			case ChannelWhatsApp, ChannelPhone, ChannelInPerson, ChannelOther:
			default:
				return nil, memErrors.ErrInvalidContactChannel
			}
			a.Channel = &v
		}
	}
	if note != nil {
		v := strings.TrimSpace(*note)
		if len(v) > 1000 {
			return nil, memErrors.ErrInvalidContactNoteTooLong
		}
		if v == "" {
			a.Note = nil
		} else {
			a.Note = &v
		}
	}
	return a, nil
}
