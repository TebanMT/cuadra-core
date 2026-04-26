package crypto

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"

	"github.com/google/uuid"
)

// GMKProvider hands the per-gym GMK to whoever needs it (sidecar reader,
// register-fingerprint use case, kiosk loop). The real implementation reads
// from the OS keychain via a Tauri command (ADR-006 §2.6); tests use the
// in-memory mock below.
type GMKProvider interface {
	GetGMK(ctx context.Context, gymID uuid.UUID) ([]byte, error)
}

// ErrGMKNotFound is returned when the provider has no key for the given gym
// (typical cause: user logged out, sidecar restarted before login).
var ErrGMKNotFound = errors.New("no GMK available for this gym (login first?)")

// InMemoryGMKProvider is a deterministic, test-friendly provider. The cloud
// build also uses it as a fallback so the cloud server can decrypt + re-encrypt
// a template during sync resolution if it ever needs to (it doesn't in MVP, but
// keeps the cross-binary API symmetric).
//
// NOT FOR PRODUCTION SIDECARS — those must use the OS keychain provider so the
// key never lives in process memory longer than necessary.
type InMemoryGMKProvider struct {
	mu   sync.RWMutex
	keys map[uuid.UUID][]byte
}

// NewInMemoryGMKProvider builds an empty provider. Seed it with Set or
// SetDeterministic before calling GetGMK.
func NewInMemoryGMKProvider() *InMemoryGMKProvider {
	return &InMemoryGMKProvider{keys: make(map[uuid.UUID][]byte)}
}

// Set stores a GMK for the given gym. Returns ErrInvalidGMK if it isn't 32 bytes.
func (p *InMemoryGMKProvider) Set(gymID uuid.UUID, gmk []byte) error {
	if len(gmk) != GMKSize {
		return ErrInvalidGMK
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]byte, GMKSize)
	copy(cp, gmk)
	p.keys[gymID] = cp
	return nil
}

// SetDeterministic derives a 32-byte GMK from sha256(gymID || seed) so tests
// can recreate the same key across runs without persisting anything.
func (p *InMemoryGMKProvider) SetDeterministic(gymID uuid.UUID, seed string) {
	h := sha256.New()
	h.Write(gymID[:])
	h.Write([]byte(seed))
	_ = p.Set(gymID, h.Sum(nil))
}

func (p *InMemoryGMKProvider) GetGMK(_ context.Context, gymID uuid.UUID) ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	k, ok := p.keys[gymID]
	if !ok {
		return nil, ErrGMKNotFound
	}
	out := make([]byte, len(k))
	copy(out, k)
	return out, nil
}

// Forget evicts the GMK for a gym (e.g. on logout). It zeroes the underlying
// slice before unmapping for defense in depth.
func (p *InMemoryGMKProvider) Forget(gymID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k, ok := p.keys[gymID]; ok {
		Zero(k)
		delete(p.keys, gymID)
	}
}
