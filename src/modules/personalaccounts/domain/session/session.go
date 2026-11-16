package session

import (
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/google/uuid"
)

const InactivityTTL = 90 * 24 * time.Hour

type Session struct {
	ID                   uuid.UUID
	AccountID            uuid.UUID
	DeviceID             uuid.UUID
	RefreshHash          [32]byte
	RefreshFamilyID      uuid.UUID
	Generation           int64
	ExpiresAt            time.Time
	RevocationTicketHash [32]byte
	RevokedAt            *time.Time
	LastUsedAt           *time.Time
	CreatedAt            time.Time
}

// NewSession starts generation one. A later verified refresh must rotate its
// token and extend inactivity together; ordinary reads never extend this TTL.
func NewSession(accountID, deviceID, sessionID uuid.UUID, refreshHash [32]byte, familyID uuid.UUID, ticketHash [32]byte, now time.Time) (*Session, error) {
	now = now.UTC()
	s := &Session{ID: sessionID, AccountID: accountID, DeviceID: deviceID, RefreshHash: refreshHash, RefreshFamilyID: familyID, Generation: 1, ExpiresAt: now.Add(InactivityTTL), RevocationTicketHash: ticketHash, CreatedAt: now}
	return s, s.Validate()
}

func validID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}

func (s *Session) Validate() error {
	if s == nil || !validID(s.ID) || !validID(s.AccountID) || !validID(s.DeviceID) || !validID(s.RefreshFamilyID) || s.RefreshHash == [32]byte{} || s.RevocationTicketHash == [32]byte{} || s.RefreshHash == s.RevocationTicketHash || s.Generation < 1 || s.CreatedAt.IsZero() || !s.ExpiresAt.After(s.CreatedAt) {
		return personalErrors.ErrInvalidSession
	}
	if (s.RevokedAt != nil && (s.RevokedAt.IsZero() || s.RevokedAt.Before(s.CreatedAt))) || (s.LastUsedAt != nil && (s.LastUsedAt.IsZero() || s.LastUsedAt.Before(s.CreatedAt) || !s.LastUsedAt.Before(s.ExpiresAt))) {
		return personalErrors.ErrInvalidSession
	}
	return nil
}

func (s *Session) ActiveAt(now time.Time) bool {
	return s.Validate() == nil && !now.IsZero() && !now.Before(s.CreatedAt) && now.Before(s.ExpiresAt) && s.RevokedAt == nil && (s.LastUsedAt == nil || !now.Before(*s.LastUsedAt))
}

func (s *Session) Revoke(now time.Time) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if now.IsZero() || now.Before(s.CreatedAt) {
		return personalErrors.ErrInvalidSession
	}
	if s.RevokedAt != nil {
		return nil
	}
	now = now.UTC()
	s.RevokedAt = &now
	return nil
}
