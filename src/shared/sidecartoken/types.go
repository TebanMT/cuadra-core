package sidecartoken

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned by Store.LookupActiveByHash and FindActive when
// no matching row exists. Callers map this to 401 in the middleware path
// and to "issue new" in the bootstrap path.
var ErrNotFound = errors.New("sidecartoken: credential not found")

// Credential is the row shape returned by Store reads. We omit key_hash
// because callers never need it after the lookup.
type Credential struct {
	ID          uuid.UUID
	GymID       uuid.UUID
	ClientID    uuid.UUID
	UserID      uuid.UUID
	DeviceLabel string
	CreatedAt   time.Time
	LastSeenAt  time.Time
}

// Store is the persistence surface for sidecar credentials. The Postgres
// implementation lives in store.go (server build tag); the interface
// itself is build-agnostic so the controller types under shared paths can
// reference it without forcing a tag.
type Store interface {
	// LookupActiveByHash resolves a token by its SHA-256 hash. Returns
	// ErrNotFound when the row is missing or revoked.
	LookupActiveByHash(ctx context.Context, hash []byte) (Credential, error)

	// TouchLastSeen updates last_seen_at to NOW(). Called after every
	// successful sidecar request — fire-and-forget OK.
	TouchLastSeen(ctx context.Context, credentialID uuid.UUID) error

	// FindActive returns the active credential for (gym_id, client_id), or
	// ErrNotFound if none exists. Used by the auth bootstrap to decide
	// whether to mint a fresh token or skip emission.
	FindActive(ctx context.Context, gymID, clientID uuid.UUID) (Credential, error)

	// ListActiveByGym returns every non-revoked credential for a gym, sorted
	// by last_seen_at desc (most-recently-used first). Drives the dashboard
	// "dispositivos vinculados" surface. Returns an empty slice (not error)
	// when the gym has zero pairings.
	ListActiveByGym(ctx context.Context, gymID uuid.UUID) ([]Credential, error)

	// Insert persists a freshly minted credential. Caller is responsible
	// for revoking any prior active row for (gym_id, client_id) first; the
	// unique partial index will reject duplicates otherwise.
	Insert(ctx context.Context, gymID, clientID, userID uuid.UUID, hash []byte, deviceLabel string) (Credential, error)

	// RevokeActive marks the active credential for (gym_id, client_id) as
	// revoked. No-op if no active row exists.
	RevokeActive(ctx context.Context, gymID, clientID uuid.UUID) error

	// RevokeIdle marks every credential whose last_seen_at < threshold as
	// revoked. Returns the number of rows revoked. Driven by the daily
	// cron in cmd/server (ADR-008 §3.5).
	RevokeIdle(ctx context.Context, threshold time.Time) (int64, error)
}
