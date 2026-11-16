package session

import (
	"math"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
)

// Rotate keeps identity and family while replacing both opaque credentials.
// The caller must retain the old ticket's limited logout permission in history.
func (s *Session) Rotate(refreshHash, ticketHash [32]byte, now time.Time) error {
	if !s.ActiveAt(now) || s.Generation == math.MaxInt64 || refreshHash == [32]byte{} || ticketHash == [32]byte{} ||
		refreshHash == ticketHash || refreshHash == s.RefreshHash || ticketHash == s.RevocationTicketHash ||
		refreshHash == s.RevocationTicketHash || ticketHash == s.RefreshHash {
		return personalErrors.ErrInvalidSession
	}
	now = now.UTC()
	s.Generation++
	s.RefreshHash, s.RevocationTicketHash = refreshHash, ticketHash
	s.LastUsedAt, s.ExpiresAt = &now, now.Add(InactivityTTL)
	return nil
}
