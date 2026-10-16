package domain

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

type tenantKey struct{}
type tenantScope struct {
	ID      uuid.UUID
	Invalid bool
}

var ErrTenantScope = errors.New("contexto de gimnasio inválido")

// WithTenant binds a trusted identity (JWT, credential or resolved auth token).
// A nested operation cannot silently switch to another gym.
func WithTenant(ctx context.Context, id uuid.UUID) context.Context {
	previous, _ := ctx.Value(tenantKey{}).(tenantScope)
	invalid := previous.Invalid || id == uuid.Nil || previous.ID != uuid.Nil && previous.ID != id
	return context.WithValue(ctx, tenantKey{}, tenantScope{ID: id, Invalid: invalid})
}
func TenantID(ctx context.Context) uuid.UUID {
	scope, _ := ctx.Value(tenantKey{}).(tenantScope)
	return scope.ID
}
func tenantInvalid(ctx context.Context) bool {
	scope, _ := ctx.Value(tenantKey{}).(tenantScope)
	return scope.Invalid
}

// CloseQuery releases an optional PostgreSQL read transaction. SQLite and
// in-memory repositories have no pinned connection to release.
func CloseQuery(tx Transaction) {
	if closer, ok := tx.(interface{ CloseQuery() }); ok {
		closer.CloseQuery()
	}
}

type tenantResolver interface {
	ResolveTenant(context.Context, string, string) (context.Context, error)
}

func ResolveTenant(ctx context.Context, uow UnitOfWork, kind, key string) (context.Context, error) {
	if resolver, ok := uow.(tenantResolver); ok {
		return resolver.ResolveTenant(ctx, kind, key)
	}
	return ctx, nil
}

type tenantIterator interface {
	ForEachTenant(context.Context, func(context.Context) (int, error)) (int, error)
}

// TenantWorker preserves the common application layer. Cloud enumerates only
// IDs, then runs the existing worker in independent, tenant-scoped transactions.
func TenantWorker(ctx context.Context, uow UnitOfWork, fn func(context.Context) (int, error)) (bool, int, error) {
	if TenantID(ctx) != uuid.Nil {
		return false, 0, nil
	}
	if iterator, ok := uow.(tenantIterator); ok {
		n, err := iterator.ForEachTenant(ctx, fn)
		return true, n, err
	}
	return false, 0, nil
}
