package device

import (
	"strings"
	"time"
	"unicode/utf8"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/google/uuid"
)

const (
	RevokeOthers = "others"
	RevokeAll    = "all"
)

// Exactly one device ID or one named scope. The current device comes from the
// authenticated actor; callers cannot choose which device "others" preserves.
type RevocationTarget struct {
	DeviceID uuid.UUID
	Scope    string
}

func (t RevocationTarget) Validate() error {
	if t.DeviceID != uuid.Nil {
		if !validID(t.DeviceID) || t.Scope != "" {
			return personalErrors.ErrInvalidDevice
		}
		return nil
	}
	if t.Scope != RevokeOthers && t.Scope != RevokeAll {
		return personalErrors.ErrInvalidDevice
	}
	return nil
}

type ListPosition struct {
	AuthorizedAt time.Time
	ID           uuid.UUID
}

func (p ListPosition) Validate() error {
	if !validID(p.ID) || !managementTimeValid(p.AuthorizedAt) {
		return personalErrors.ErrInvalidDevice
	}
	return nil
}

// ListedDevice is a deliberately narrow projection. No public keys, provider
// identifiers, session hashes, refresh tokens or signing material are loaded.
type ListedDevice struct {
	ID           uuid.UUID
	AccountID    uuid.UUID
	Platform     string
	Label        string
	AuthorizedAt time.Time
	RevokedAt    *time.Time
	LastSeenAt   *time.Time
}

func (d ListedDevice) Validate() error {
	if !validID(d.ID) || !validID(d.AccountID) || (d.Platform != "ios" && d.Platform != "android") || !utf8.ValidString(d.Label) ||
		strings.ContainsRune(d.Label, 0) || utf8.RuneCountInString(d.Label) < 1 || utf8.RuneCountInString(d.Label) > 80 || !managementTimeValid(d.AuthorizedAt) {
		return personalErrors.ErrInvalidDevice
	}
	for _, at := range []*time.Time{d.RevokedAt, d.LastSeenAt} {
		if at != nil && (!managementTimeValid(*at) || at.Before(d.AuthorizedAt)) {
			return personalErrors.ErrInvalidDevice
		}
	}
	return nil
}

func managementTimeValid(at time.Time) bool {
	return !at.IsZero() && at.UTC().Year() >= 1 && at.UTC().Year() <= 9999
}
