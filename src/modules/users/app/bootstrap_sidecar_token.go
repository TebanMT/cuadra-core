package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/sidecartoken"
)

// BootstrapSidecarTokenInput is what the auth controller passes after a
// successful login/signup (ADR-008 §3.3). When the request did NOT come
// from a sidecar (X-Cuadra-Client absent or non-sidecar), the controller
// skips the bootstrap entirely.
type BootstrapSidecarTokenInput struct {
	GymID       uuid.UUID
	UserID      uuid.UUID
	ClientID    uuid.UUID
	DeviceLabel string
}

// BootstrapSidecarToken is the cloud-side handshake that turns a fresh
// login from a sidecar into a long-lived `sk_live_*` credential.
// Idempotent per (gym_id, client_id): if an active credential already
// exists, it returns an empty token so the sidecar keeps using its
// previously stored one. The current sidecar's token never reaches the
// cloud during login (it talks via the JWT path), so we cannot detect
// "different machine reusing the same client_id" — but the unique partial
// index on (gym_id, client_id) WHERE revoked_at IS NULL guarantees only
// one active row, so the worst case is a no-op.
type BootstrapSidecarToken struct {
	Store sidecartoken.Store
	UoW   sharedDomain.UnitOfWork
}

func NewBootstrapSidecarToken(store sidecartoken.Store, uow sharedDomain.UnitOfWork) *BootstrapSidecarToken {
	return &BootstrapSidecarToken{Store: store, UoW: uow}
}

// Execute returns the plaintext sidecar_token to include in the login
// response (or "" when the sidecar already has an active credential and
// shouldn't replace it). Errors are non-fatal to the caller — they should
// log + continue (login already succeeded; the sidecar will keep using
// the JWT until its next login attempt).
func (uc *BootstrapSidecarToken) Execute(ctx context.Context, in BootstrapSidecarTokenInput) (string, error) {
	if uc.Store == nil {
		return "", nil
	}
	if in.GymID == uuid.Nil || in.UserID == uuid.Nil || in.ClientID == uuid.Nil {
		return "", fmt.Errorf("bootstrap: missing required ids")
	}
	// First check (no transaction needed for the read).
	if _, err := uc.Store.FindActive(ctx, in.GymID, in.ClientID); err == nil {
		return "", nil
	} else if !errors.Is(err, sidecartoken.ErrNotFound) {
		return "", err
	}
	// Mint + insert. Done outside any larger transaction; the caller has
	// already committed login by this point and a partial bootstrap is
	// safe to retry on the sidecar's next login.
	plaintext, hash, err := sidecartoken.Generate()
	if err != nil {
		return "", err
	}
	if _, err := uc.Store.Insert(ctx, in.GymID, in.ClientID, in.UserID, hash, in.DeviceLabel); err != nil {
		return "", err
	}
	return plaintext, nil
}
