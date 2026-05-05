package controllers

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// whatsappOTPStore is a small in-memory map of (gym_id) → OTP entry. Backs
// the 2-step WhatsApp connect flow (UC-037 minimal MVP). Production should
// migrate to Redis/SQL with rate-limiting; for now in-memory is fine — the
// blast radius is "the owner has to retry /start", and the secret never
// leaves the host process.
type whatsappOTPStore struct {
	mu      sync.Mutex
	entries map[uuid.UUID]otpEntry
	ttl     time.Duration
}

type otpEntry struct {
	Phone     string
	Code      string
	ExpiresAt time.Time
}

func newWhatsappOTPStore(ttl time.Duration) *whatsappOTPStore {
	return &whatsappOTPStore{entries: map[uuid.UUID]otpEntry{}, ttl: ttl}
}

// Issue generates and stores a 6-digit code for `gymID`/`phone`. Overwrites
// any prior outstanding code so the operator can re-trigger /start without
// the previous code lingering. Returns the (code, expiresAt).
func (s *whatsappOTPStore) Issue(gymID uuid.UUID, phone string) (string, time.Time, error) {
	code, err := generate6DigitOTP()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().UTC().Add(s.ttl)
	s.mu.Lock()
	s.entries[gymID] = otpEntry{Phone: phone, Code: code, ExpiresAt: exp}
	s.mu.Unlock()
	return code, exp, nil
}

// Verify returns true iff the (gym_id, phone, code) tuple matches an
// unexpired entry. On success the entry is consumed (single-use).
func (s *whatsappOTPStore) Verify(gymID uuid.UUID, phone, code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[gymID]
	if !ok {
		return false
	}
	if time.Now().UTC().After(e.ExpiresAt) {
		delete(s.entries, gymID)
		return false
	}
	if e.Phone != phone || e.Code != code {
		return false
	}
	delete(s.entries, gymID)
	return true
}

func generate6DigitOTP() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return fmt.Sprintf("%06d", n%1_000_000), nil
}
