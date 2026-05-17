//go:build sidecar

package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"
)

// KeyringGMKProvider stores the per-gym GMK in the OS-level secret store
// (Windows Credential Manager, macOS Keychain, libsecret on Linux) so the
// key survives sidecar restarts without ever being persisted to SQLite.
//
// ADR-006 §2.6 calls for the GMK to live outside the SQLite file precisely
// because that file gets backed up / pushed to cloud as part of the gym's
// data; leaking the GMK there would defeat the whole encrypted-template
// scheme. The OS keychain gives us per-user isolation on Windows (the
// realistic gym kiosk) and per-bundle scoping on macOS (dev).
//
// Lifecycle:
//
//   - First GetGMK for an unknown gym → generate a fresh 256-bit key with
//     crypto/rand, persist it under "cuadra.gmk" / "<gym_id>", return it.
//     Subsequent calls read from keyring.
//   - Logout / gym pairing change → caller can Forget(gymID) to drop the
//     entry. Same shape as InMemoryGMKProvider.Forget so the sidecar wiring
//     can swap providers without other code paths caring.
//   - In-process cache mirrors what's in the keyring so the hot path (each
//     fingerprint match decrypts the whole gallery) doesn't shell out to
//     /usr/bin/security or DPAPI on every template.
//
// The provider is sidecar-only: the cloud build keeps using the in-memory
// provider — server boxes don't have a per-user keychain in the relevant
// sense, and the cloud doesn't decrypt templates today anyway.
type KeyringGMKProvider struct {
	service string

	mu    sync.RWMutex
	cache map[uuid.UUID][]byte
}

// keyringServiceName is the namespace under which all per-gym GMKs live in
// the OS secret store. Stable so an installer upgrade keeps reading the same
// entries — bumping it would orphan every enrolled gym's key.
const keyringServiceName = "cuadra.gmk"

// NewKeyringGMKProvider returns a provider backed by the OS keyring. It
// doesn't touch the keyring on construction — the first call to GetGMK is
// what triggers either a lookup or a fresh-key generation.
func NewKeyringGMKProvider() *KeyringGMKProvider {
	return &KeyringGMKProvider{
		service: keyringServiceName,
		cache:   make(map[uuid.UUID][]byte),
	}
}

// GetGMK satisfies the GMKProvider contract. Lookup order:
//
//  1. In-process cache (avoids hitting the OS keychain on every checkin).
//  2. OS keyring under (service, gym_id.String()).
//  3. Generate a new 32-byte key, persist to keyring, cache, return.
//
// The returned slice is a copy — callers are free to Zero() it without
// affecting the cached or stored value.
func (p *KeyringGMKProvider) GetGMK(_ context.Context, gymID uuid.UUID) ([]byte, error) {
	if gymID == uuid.Nil {
		return nil, ErrGMKNotFound
	}

	p.mu.RLock()
	if cached, ok := p.cache[gymID]; ok {
		out := make([]byte, len(cached))
		copy(out, cached)
		p.mu.RUnlock()
		return out, nil
	}
	p.mu.RUnlock()

	// Slow path under the write lock so concurrent first-time matches for
	// the same gym don't race and create two different GMKs (the first one
	// to win the store would orphan every template the loser encrypted).
	p.mu.Lock()
	defer p.mu.Unlock()
	if cached, ok := p.cache[gymID]; ok {
		out := make([]byte, len(cached))
		copy(out, cached)
		return out, nil
	}

	gmk, err := p.loadOrCreateLocked(gymID)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(gmk))
	copy(out, gmk)
	return out, nil
}

// Forget drops the cached + keyring entry for a gym. Called on logout/unpair
// so the next operator on the same physical PC can't passively reach the
// previous tenant's templates. Errors talking to the keyring are logged-only
// (no return) — we already cleared the in-memory copy, which is the more
// urgent half.
func (p *KeyringGMKProvider) Forget(gymID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if k, ok := p.cache[gymID]; ok {
		Zero(k)
		delete(p.cache, gymID)
	}
	_ = keyring.Delete(p.service, gymID.String())
}

// loadOrCreateLocked must be called with p.mu held. Returns the in-cache
// pointer (NOT a copy — caller must copy before exposing).
func (p *KeyringGMKProvider) loadOrCreateLocked(gymID uuid.UUID) ([]byte, error) {
	encoded, err := keyring.Get(p.service, gymID.String())
	switch {
	case err == nil:
		raw, derr := base64.StdEncoding.DecodeString(encoded)
		if derr != nil {
			// Corrupted entry — refuse rather than silently overwriting,
			// since overwriting would orphan whatever templates the prior
			// GMK encrypted. Operator can `Forget` + re-pair.
			return nil, fmt.Errorf("keyring entry for gym %s is malformed: %w", gymID, derr)
		}
		if len(raw) != GMKSize {
			return nil, fmt.Errorf("keyring entry for gym %s has wrong length (%d != %d)", gymID, len(raw), GMKSize)
		}
		p.cache[gymID] = raw
		return raw, nil
	case errors.Is(err, keyring.ErrNotFound):
		// First time we see this gym on this device. Generate.
		fresh := make([]byte, GMKSize)
		if _, rerr := rand.Read(fresh); rerr != nil {
			return nil, fmt.Errorf("generate gmk for gym %s: %w", gymID, rerr)
		}
		if serr := keyring.Set(p.service, gymID.String(), base64.StdEncoding.EncodeToString(fresh)); serr != nil {
			// Persisting to the OS keychain failed — common failure modes
			// on Windows are corp policies blocking the credential manager
			// or the Lite Client installer corrupting WinCred. Return the
			// error rather than caching, so the next attempt retries the
			// write (operator can fix policy / reinstall and proceed).
			return nil, fmt.Errorf("persist gmk for gym %s to keyring: %w", gymID, serr)
		}
		p.cache[gymID] = fresh
		return fresh, nil
	default:
		return nil, fmt.Errorf("read gmk for gym %s: %w", gymID, err)
	}
}
