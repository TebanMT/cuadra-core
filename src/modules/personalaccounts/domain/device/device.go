package device

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/google/uuid"
)

type AuthorizationMethod string

const (
	Signup   AuthorizationMethod = "signup"
	Device   AuthorizationMethod = "device"
	Recovery AuthorizationMethod = "recovery"
)

type AuthorizedDevice struct {
	ID                 uuid.UUID
	AccountID          uuid.UUID
	Platform           string
	Label              string
	PublicKey          [32]byte
	AuthorizedAt       time.Time
	AuthorizedByMethod AuthorizationMethod
	RevokedAt          *time.Time
	RevocationReason   string
	LastSeenAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	DeletedAt          *time.Time
}

// Authorization must already have been proven by the use case. This copies
// the exact persisted candidate without changing its ID, key, platform or label.
func NewAuthorizedDevice(accountID uuid.UUID, candidate attempt.Candidate, method AuthorizationMethod, now time.Time) (*AuthorizedDevice, error) {
	now = now.UTC()
	d := &AuthorizedDevice{ID: candidate.ID, AccountID: accountID, Platform: candidate.Platform, Label: candidate.Label, PublicKey: candidate.PublicKey, AuthorizedAt: now, AuthorizedByMethod: method, CreatedAt: now, UpdatedAt: now}
	return d, d.Validate()
}

func validID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}

func (d *AuthorizedDevice) Validate() error {
	if d == nil || !validID(d.ID) || !validID(d.AccountID) || d.PublicKey == [32]byte{} || (d.Platform != "ios" && d.Platform != "android") ||
		!utf8.ValidString(d.Label) || strings.ContainsRune(d.Label, 0) || utf8.RuneCountInString(d.Label) < 1 || utf8.RuneCountInString(d.Label) > 80 ||
		d.CreatedAt.IsZero() || d.AuthorizedAt.Before(d.CreatedAt) || d.UpdatedAt.Before(d.CreatedAt) ||
		(d.AuthorizedByMethod != Signup && d.AuthorizedByMethod != Device && d.AuthorizedByMethod != Recovery) {
		return personalErrors.ErrInvalidDevice
	}
	if d.RevokedAt != nil && (d.RevokedAt.IsZero() || d.RevokedAt.Before(d.AuthorizedAt) || strings.TrimSpace(d.RevocationReason) == "") {
		return personalErrors.ErrInvalidDevice
	}
	if (d.RevokedAt == nil && d.RevocationReason != "") || !utf8.ValidString(d.RevocationReason) || strings.ContainsRune(d.RevocationReason, 0) ||
		(d.LastSeenAt != nil && (d.LastSeenAt.IsZero() || d.LastSeenAt.Before(d.AuthorizedAt))) || (d.DeletedAt != nil && (d.DeletedAt.IsZero() || d.DeletedAt.Before(d.CreatedAt))) {
		return personalErrors.ErrInvalidDevice
	}
	return nil
}

func (d *AuthorizedDevice) ActiveAt(now time.Time) bool {
	return d.Validate() == nil && !now.IsZero() && !now.Before(d.AuthorizedAt) && d.RevokedAt == nil && d.DeletedAt == nil
}

func (d *AuthorizedDevice) Revoke(now time.Time, reason string) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if now.IsZero() || now.Before(d.AuthorizedAt) || strings.TrimSpace(reason) == "" || !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) {
		return personalErrors.ErrInvalidDevice
	}
	if d.RevokedAt != nil {
		return nil
	}
	if d.DeletedAt != nil {
		return personalErrors.ErrAuthObjectUnavailable
	}
	now = now.UTC()
	d.RevokedAt, d.RevocationReason, d.UpdatedAt = &now, reason, now
	return nil
}
