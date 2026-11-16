package domain

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// PersonalRole selects one of the separately credentialed Mancuerna pools.
type PersonalRole string

// PersonalSnapshotUnitOfWork is a strict materialization capability. Unlike
// report ReadSnapshot, it has no weaker fallback: all pages and the manifest
// must be committed from one repeatable-read view under the account mutex.
type PersonalSnapshotUnitOfWork interface {
	SnapshotCommand(context.Context, func(Transaction) error) error
}

// PersonalAuthPreparedUnitOfWork preserves the global budget -> phone ->
// account lock order for authenticated identity changes. Prepare is trusted
// application code that only reserves capacity or acquires prerequisite locks;
// authority changes belong in apply, after the account mutex is acquired.
// Both callbacks share one transaction and roll back together on any error.
type PersonalAuthPreparedUnitOfWork interface {
	PreparedCommand(context.Context, func(Transaction) error, func(Transaction) error) error
}

const (
	PersonalDataRole PersonalRole = "mancuerna_data"
	PersonalAuthRole PersonalRole = "mancuerna_auth"
)

var (
	ErrPersonalScope         = errors.New("contexto personal inválido")
	ErrPersonalStreamMissing = errors.New("cuenta personal sin canal de sincronización")
)

type personalKey struct{}
type personalScope struct {
	AccountID   uuid.UUID
	AttemptID   uuid.UUID
	AttemptHash [32]byte
	Invalid     bool
}

// WithPersonalAccount binds an already trusted personal identity. It cannot
// switch an existing account scope or inherit an administrative gym scope.
func WithPersonalAccount(ctx context.Context, id uuid.UUID) context.Context {
	scope := personalContext(ctx)
	scope.Invalid = scope.Invalid || id == uuid.Nil || TenantID(ctx) != uuid.Nil || tenantScopeInvalid(ctx) ||
		(scope.AccountID != uuid.Nil && scope.AccountID != id)
	scope.AccountID = id
	return context.WithValue(ctx, personalKey{}, scope)
}

// WithPersonalAttempt scopes pre-login access to a single attempt and the
// SHA-256 of its continuation secret. Never pass the plaintext secret here.
// Adding an account later is allowed after the auth use case verifies it.
func WithPersonalAttempt(ctx context.Context, id uuid.UUID, continuationHash [32]byte) context.Context {
	scope := personalContext(ctx)
	scope.Invalid = scope.Invalid || id == uuid.Nil || continuationHash == [32]byte{} ||
		TenantID(ctx) != uuid.Nil || tenantScopeInvalid(ctx) ||
		(scope.AttemptID != uuid.Nil && (scope.AttemptID != id || scope.AttemptHash != continuationHash))
	scope.AttemptID, scope.AttemptHash = id, continuationHash
	return context.WithValue(ctx, personalKey{}, scope)
}

func PersonalAccountID(ctx context.Context) uuid.UUID { return personalContext(ctx).AccountID }

// WithAuthenticatedPersonalAccount scopes an already verified session actor.
// Unlike a signup finalizer, a session endpoint must never inherit an Attempt
// capability, even when it uses the auth database pool for security operations.
func WithAuthenticatedPersonalAccount(ctx context.Context, id uuid.UUID) (context.Context, error) {
	if ctx == nil {
		return nil, ErrPersonalScope
	}
	scoped := WithPersonalAccount(ctx, id)
	if err := validatePersonalScope(scoped, PersonalDataRole); err != nil {
		return nil, err
	}
	return scoped, nil
}

// ValidatePersonalAnonymousScope is the entry boundary for fixed credential
// resolvers such as refresh/logout. An ambient tenant or attempt capability
// must not reach their identity lookup, even when its visible UUID is empty.
func ValidatePersonalAnonymousScope(ctx context.Context) error {
	if ctx == nil || personalContextPresent(ctx) || TenantID(ctx) != uuid.Nil || tenantScopeInvalid(ctx) {
		return ErrPersonalScope
	}
	return nil
}

func personalContext(ctx context.Context) personalScope {
	scope, _ := ctx.Value(personalKey{}).(personalScope)
	return scope
}

func personalContextPresent(ctx context.Context) bool {
	_, present := ctx.Value(personalKey{}).(personalScope)
	return present
}

func validatePersonalScope(ctx context.Context, role PersonalRole) error {
	scope := personalContext(ctx)
	if scope.Invalid || TenantID(ctx) != uuid.Nil || tenantScopeInvalid(ctx) {
		return ErrPersonalScope
	}
	switch role {
	case PersonalDataRole:
		if scope.AccountID == uuid.Nil || scope.AttemptID != uuid.Nil {
			return ErrPersonalScope
		}
	case PersonalAuthRole:
		if scope.AccountID == uuid.Nil && scope.AttemptID == uuid.Nil {
			return ErrPersonalScope
		}
	default:
		return ErrPersonalScope
	}
	return nil
}
